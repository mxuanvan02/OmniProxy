package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bankResetUpstream serves the two endpoints a Codex usage refresh touches:
// the probe POST that carries the rate-limit headers, and the separate GET that
// reports how many bank-reset credits the account holds.
//
// probeStatus lets a test make the probe answer 429 — the state an account is in
// precisely when an operator goes looking for a reset credit.
func bankResetUpstream(t *testing.T, probeStatus int, creditsBody string, creditsStatus int) (*httptest.Server, *int) {
	t.Helper()
	creditsCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/backend-api/wham/usage"):
			creditsCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(creditsStatus)
			_, _ = w.Write([]byte(creditsBody))
		case strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses"):
			// Usage headers arrive on any status, including 429.
			w.Header().Set("x-codex-primary-used-percent", "0")
			w.Header().Set("x-codex-secondary-used-percent", "100")
			w.WriteHeader(probeStatus)
			if probeStatus == 429 {
				_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached"}}`))
				return
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	return srv, &creditsCalls
}

// The bug this pins: the bank-reset credit count was only read on the probe's
// 200 path, but a bank-reset credit exists to rescue an account whose quota is
// spent — and a spent account answers the probe with 429. The refresh returned
// early, the count stayed 0, and the UI disabled the one button that would have
// fixed the account. Observed live: probe 429 while the credits endpoint
// reported available_count=2, with the dashboard showing "Bank Reset 0".
func TestRateLimitedAccountStillLearnsItsBankResetCredits(t *testing.T) {
	srv, creditsCalls := bankResetUpstream(t, 429, `{"rate_limit_reset_credits":{"available_count":2}}`, 200)
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)

	// 429 is not a hard failure: the headers were captured, so the refresh
	// reports success.
	if err := fetchCodexUsageAttempt(acc); err != nil {
		t.Fatalf("rate-limited probe should not be a hard error, got: %v", err)
	}
	if *creditsCalls == 0 {
		t.Fatal("the credits endpoint was never called on the rate-limited path — " +
			"this is the bug: the account cannot discover the credit that would unblock it")
	}
	if acc.CodexResetCreditsAvailable != 2 {
		t.Fatalf("cached bank-reset credits = %d, want 2; a zero here disables the button upstream says is usable",
			acc.CodexResetCreditsAvailable)
	}
}

// The healthy path must keep working: a 200 probe still refreshes the count.
func TestHealthyAccountAlsoRefreshesBankResetCredits(t *testing.T) {
	srv, creditsCalls := bankResetUpstream(t, 200, `{"rate_limit_reset_credits":{"available_count":1}}`, 200)
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)
	if err := fetchCodexUsageAttempt(acc); err != nil {
		t.Fatalf("healthy probe failed: %v", err)
	}
	if *creditsCalls != 1 {
		t.Fatalf("credits endpoint called %d times, want exactly 1", *creditsCalls)
	}
	if acc.CodexResetCreditsAvailable != 1 {
		t.Fatalf("cached bank-reset credits = %d, want 1", acc.CodexResetCreditsAvailable)
	}
}

// A failed credit lookup must leave the previously known value alone. Zeroing it
// would disable the button on a transient blip, which is the same
// operator-visible failure as the original bug.
func TestFailedCreditLookupKeepsTheLastKnownCount(t *testing.T) {
	srv, _ := bankResetUpstream(t, 429, `{"error":"upstream unavailable"}`, 503)
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)
	acc.CodexResetCreditsAvailable = 3 // learned on an earlier successful poll

	if err := fetchCodexUsageAttempt(acc); err != nil {
		t.Fatalf("probe should still report success: %v", err)
	}
	if acc.CodexResetCreditsAvailable != 3 {
		t.Fatalf("cached count = %d after a failed lookup, want the last known 3 preserved",
			acc.CodexResetCreditsAvailable)
	}
}

// Zero must still be recorded as zero: once the credits are genuinely spent the
// button has to go back to disabled rather than keeping a stale positive count.
func TestSpentCreditsAreRecordedAsZero(t *testing.T) {
	srv, _ := bankResetUpstream(t, 200, `{"rate_limit_reset_credits":{"available_count":0}}`, 200)
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)
	acc.CodexResetCreditsAvailable = 2

	if err := fetchCodexUsageAttempt(acc); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if acc.CodexResetCreditsAvailable != 0 {
		t.Fatalf("cached count = %d, want 0 once upstream reports none left",
			acc.CodexResetCreditsAvailable)
	}
}
