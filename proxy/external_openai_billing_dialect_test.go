package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"omniproxy/config"
)

// dashboardBillingServer serves the one-api/new-api billing dialect with a
// fixed hard_limit_usd and a consumed amount in cents.
func dashboardBillingServer(t *testing.T, hardLimitUSD, usageCents string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			_, _ = w.Write([]byte(`{"hard_limit_usd":` + hardLimitUSD +
				`,"soft_limit_usd":` + hardLimitUSD +
				`,"system_hard_limit_usd":` + hardLimitUSD + `}`))
		case "/v1/dashboard/billing/usage":
			_, _ = w.Write([]byte(`{"object":"list","total_usage":` + usageCents + `}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Some gateways answer hard_limit_usd with the key's TOTAL quota instead of its
// remaining balance. Reading a ceiling as a balance doubles the derived limit,
// so a fully-spent 60/60 key renders as 60/120 at 50% used — and the pool keeps
// routing to an account that has no money left.
//
// An operator who knows the dialect states it on the account.
func TestDashboardBillingHonoursTotalQuotaFlag(t *testing.T) {
	initConfigForTests(t)
	srv := dashboardBillingServer(t, "60", "6000")

	me, err := fetchExternalProviderCredits(&config.Account{
		AccessToken:            "k",
		BaseURL:                srv.URL,
		ExtBillingLimitIsTotal: true,
	})
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits: %v", err)
	}
	if me.CreditLimit != 60 || me.CreditsUsed != 60 || me.CreditsRemaining != 0 {
		t.Fatalf("limit/used/remaining = %v/%v/%v, want 60/60/0",
			me.CreditLimit, me.CreditsUsed, me.CreditsRemaining)
	}
}

// Without an operator flag the dialect is inferred from the account's own
// history: a balance shrinks as the key is spent, so a hard_limit_usd that
// stands still while consumption rises is a ceiling, not a balance.
func TestDashboardBillingDetectsTotalQuotaFromStalledLimit(t *testing.T) {
	initConfigForTests(t)
	srv := dashboardBillingServer(t, "60", "6000")

	// Previous refresh saw the same 60 as "remaining" when almost nothing had
	// been spent. Consumption has since reached 60 and 60 has not moved.
	account := &config.Account{
		AccessToken:         "k",
		BaseURL:             srv.URL,
		ExtCreditsRemaining: 60,
		ExtCreditsUsed:      0.000043030769,
		ExtCreditLimit:      60.000043030769,
		ExtCreditsCheckedAt: 1789228191,
	}

	me, err := fetchExternalProviderCredits(account)
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits: %v", err)
	}
	if !me.BillingLimitIsTotal {
		t.Error("stalled hard_limit_usd was not recognised as a total quota")
	}
	if me.CreditLimit != 60 || me.CreditsRemaining != 0 {
		t.Fatalf("limit/remaining = %v/%v, want 60/0", me.CreditLimit, me.CreditsRemaining)
	}
}

// The one-api semantics must survive the inference: there the balance really
// does shrink, so a changed hard_limit_usd means "remaining" and the limit is
// still derived as remaining+used. Getting this wrong parks a paying account.
func TestDashboardBillingKeepsBalanceSemanticsWhenLimitMoves(t *testing.T) {
	initConfigForTests(t)
	srv := dashboardBillingServer(t, "30", "7000")

	account := &config.Account{
		AccessToken:         "k",
		BaseURL:             srv.URL,
		ExtCreditsRemaining: 50,
		ExtCreditsUsed:      50,
		ExtCreditLimit:      100,
		ExtCreditsCheckedAt: 1789228191,
	}

	me, err := fetchExternalProviderCredits(account)
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits: %v", err)
	}
	if me.BillingLimitIsTotal {
		t.Error("a shrinking balance was misread as a total quota")
	}
	if me.CreditsRemaining != 30 || me.CreditsUsed != 70 || me.CreditLimit != 100 {
		t.Fatalf("remaining/used/limit = %v/%v/%v, want 30/70/100",
			me.CreditsRemaining, me.CreditsUsed, me.CreditLimit)
	}
}

// A first-ever refresh has no history to compare against, so it must not guess:
// one-api semantics stay the default until an observation contradicts them.
func TestDashboardBillingDoesNotGuessWithoutHistory(t *testing.T) {
	initConfigForTests(t)
	srv := dashboardBillingServer(t, "60", "6000")

	me, err := fetchExternalProviderCredits(&config.Account{AccessToken: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits: %v", err)
	}
	if me.BillingLimitIsTotal {
		t.Error("inferred a total quota from a single observation")
	}
	if me.CreditsRemaining != 60 || me.CreditLimit != 120 {
		t.Fatalf("remaining/limit = %v/%v, want 60/120 (unchanged legacy behaviour)",
			me.CreditsRemaining, me.CreditLimit)
	}
}

// An unmetered key still collapses to "no limit" under the total-quota reading,
// otherwise the sentinel would render as a real 1e8 ceiling.
func TestDashboardBillingTotalQuotaStillHandlesUnmeteredSentinel(t *testing.T) {
	initConfigForTests(t)
	srv := dashboardBillingServer(t, "100000000", "544883.83")

	me, err := fetchExternalProviderCredits(&config.Account{
		AccessToken:            "k",
		BaseURL:                srv.URL,
		ExtBillingLimitIsTotal: true,
	})
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits: %v", err)
	}
	if me.CreditLimit != 0 || me.CreditsRemaining != 0 {
		t.Fatalf("limit/remaining = %v/%v, want 0/0 for an unmetered key",
			me.CreditLimit, me.CreditsRemaining)
	}
}

// KNOWN LIMITATION, pinned deliberately.
//
// Movement-based inference needs consumption to rise between two refreshes. An
// account already recorded under the wrong reading AND already exhausted cannot
// supply that: it is refused upstream, so its spend never moves again, so the
// wrong numbers persist. This is the deadlock that makes the operator flag
// necessary rather than merely convenient.
//
// Change this test only alongside a discriminator that works from a single
// observation (an upstream no-balance verdict contradicting a stored positive
// balance is the obvious candidate).
func TestDashboardBillingCannotRecoverAnExhaustedMisreadAccount(t *testing.T) {
	initConfigForTests(t)
	srv := dashboardBillingServer(t, "60", "6000")

	// Exactly the state the bug left on disk: limit doubled to remaining+used,
	// and consumption frozen because the key is spent.
	account := &config.Account{
		AccessToken:         "k",
		BaseURL:             srv.URL,
		ExtCreditLimit:      120,
		ExtCreditsRemaining: 60,
		ExtCreditsUsed:      60,
		ExtCreditsCheckedAt: 1789271887,
	}

	me, err := fetchExternalProviderCredits(account)
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits: %v", err)
	}
	if me.BillingLimitIsTotal {
		t.Fatal("inference now recovers a frozen account — update this test and " +
			"the operator-flag guidance together")
	}
	if me.CreditLimit != 120 {
		t.Fatalf("limit = %v, want the stale 120 this limitation describes", me.CreditLimit)
	}

	// The escape hatch must actually escape.
	account.ExtBillingLimitIsTotal = true
	fixed, err := fetchExternalProviderCredits(account)
	if err != nil {
		t.Fatalf("fetchExternalProviderCredits after flag: %v", err)
	}
	if fixed.CreditLimit != 60 || fixed.CreditsRemaining != 0 {
		t.Fatalf("flagged limit/remaining = %v/%v, want 60/0",
			fixed.CreditLimit, fixed.CreditsRemaining)
	}
}

// Once inferred, the dialect must be persisted: otherwise every restart pays
// the same wrong reading until the history happens to line up again.
func TestExtBillingLimitIsTotalPersists(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "billing-dialect"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.SetAccountExtBillingLimitIsTotal("billing-dialect", true); err != nil {
		t.Fatalf("SetAccountExtBillingLimitIsTotal: %v", err)
	}
	for _, a := range config.GetAccounts() {
		if a.ID != "billing-dialect" {
			continue
		}
		if !a.ExtBillingLimitIsTotal {
			t.Fatal("flag was not persisted onto the account")
		}
		return
	}
	t.Fatal("account disappeared after the update")
}
