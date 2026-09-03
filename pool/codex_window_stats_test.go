package pool

import (
	"errors"
	"omniproxy/config"
	"path/filepath"
	"testing"
	"time"
)

// newStatsPool builds a pool with the maps UpdateStats and the Codex window
// helpers require, without touching the global singleton.
func newStatsPool(accounts ...config.Account) *AccountPool {
	return &AccountPool{
		accounts:    accounts,
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
		modelLocks:  make(map[string]map[string]time.Time),
		stats:       make(map[string]*accountStats),
	}
}

// initStatsConfig points the config singleton at a throwaway file so
// UpdateStats can persist without touching the operator's real config.
func initStatsConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

// TestUpdateStatsAccumulatesAndPersists guards the counter path that Reload
// used to clobber: stats live in p.stats keyed by account ID, and the totals
// must reach config before UpdateStats returns (no fire-and-forget write).
func TestUpdateStatsAccumulatesAndPersists(t *testing.T) {
	initStatsConfig(t)
	acct := config.Account{ID: "acc-stats", Enabled: true, AuthMethod: "social"}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newStatsPool(acct)

	p.UpdateStats("acc-stats", 100, 1.5)
	p.UpdateStats("acc-stats", 40, 0.5)

	got := p.GetAllAccounts()
	if len(got) != 1 {
		t.Fatalf("GetAllAccounts len = %d, want 1", len(got))
	}
	if got[0].RequestCount != 2 {
		t.Fatalf("RequestCount = %d, want 2", got[0].RequestCount)
	}
	if got[0].TotalTokens != 140 {
		t.Fatalf("TotalTokens = %d, want 140", got[0].TotalTokens)
	}
	if got[0].TotalCredits != 2.0 {
		t.Fatalf("TotalCredits = %v, want 2", got[0].TotalCredits)
	}
	if got[0].LastUsed == 0 {
		t.Fatal("LastUsed not stamped")
	}
	// Non-codex account: the per-window counter must stay at zero.
	if got[0].CodexTokensSincePrimaryReset != 0 {
		t.Fatalf("non-codex account accrued window tokens: %d", got[0].CodexTokensSincePrimaryReset)
	}

	// The write must have landed in config synchronously.
	for _, a := range config.GetAccounts() {
		if a.ID != "acc-stats" {
			continue
		}
		if a.TotalTokens != 140 || a.RequestCount != 2 {
			t.Fatalf("config not updated synchronously: requests=%d tokens=%d", a.RequestCount, a.TotalTokens)
		}
	}
}

// TestUpdateStatsSeedsFromPersistedSnapshot covers the branch where traffic
// arrives before Reload has seeded p.stats: the new entry must continue from
// the persisted totals rather than restarting from zero.
func TestUpdateStatsSeedsFromPersistedSnapshot(t *testing.T) {
	initStatsConfig(t)
	acct := config.Account{
		ID: "acc-seed", Enabled: true, AuthMethod: "social",
		RequestCount: 7, TotalTokens: 700, TotalCredits: 3, ErrorCount: 2,
	}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	// Pool has the account routable but p.stats is deliberately empty.
	p := newStatsPool(acct)

	p.UpdateStats("acc-seed", 50, 1)

	got := p.GetAllAccounts()[0]
	if got.RequestCount != 8 {
		t.Fatalf("RequestCount = %d, want 8 (7 persisted + 1)", got.RequestCount)
	}
	if got.TotalTokens != 750 {
		t.Fatalf("TotalTokens = %d, want 750 (700 persisted + 50)", got.TotalTokens)
	}
	if got.ErrorCount != 2 {
		t.Fatalf("ErrorCount = %d, want 2 preserved from snapshot", got.ErrorCount)
	}
}

// TestUpdateStatsTracksCodexWindowTokens pins isCodexAccountLocked: only a
// codex account accrues the per-window token counter that drives quota
// rotation.
func TestUpdateStatsTracksCodexWindowTokens(t *testing.T) {
	initStatsConfig(t)
	codex := config.Account{ID: "acc-codex", Enabled: true, AuthMethod: "codex"}
	social := config.Account{ID: "acc-social", Enabled: true, AuthMethod: "social"}
	for _, a := range []config.Account{codex, social} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}
	p := newStatsPool(codex, social)

	p.UpdateStats("acc-codex", 120, 0)
	p.UpdateStats("acc-social", 120, 0)

	byID := map[string]config.Account{}
	for _, a := range p.GetAllAccounts() {
		byID[a.ID] = a
	}
	if byID["acc-codex"].CodexTokensSincePrimaryReset != 120 {
		t.Fatalf("codex window tokens = %d, want 120", byID["acc-codex"].CodexTokensSincePrimaryReset)
	}
	if byID["acc-social"].CodexTokensSincePrimaryReset != 0 {
		t.Fatalf("social account window tokens = %d, want 0", byID["acc-social"].CodexTokensSincePrimaryReset)
	}
}

// TestUpdateStatsResolvesCodexAuthMethodFromConfig covers the second lookup in
// isCodexAccountLocked: an account absent from the routing slice (for example a
// service-only or freshly disabled account) still resolves via config.
func TestUpdateStatsResolvesCodexAuthMethodFromConfig(t *testing.T) {
	initStatsConfig(t)
	if err := config.AddAccount(config.Account{ID: "acc-offpool", Enabled: true, AuthMethod: "codex"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newStatsPool() // empty routing slice on purpose

	p.UpdateStats("acc-offpool", 90, 0)

	full := map[string]config.Account{}
	for _, a := range p.GetAllAccountsFull() {
		full[a.ID] = a
	}
	if full["acc-offpool"].CodexTokensSincePrimaryReset != 90 {
		t.Fatalf("window tokens = %d, want 90 (authMethod resolved from config)",
			full["acc-offpool"].CodexTokensSincePrimaryReset)
	}
}

// TestResetCodexPrimaryWindowTokensZeroesCounterAndStampsDeadline guards the
// upstream-reset path: the live per-window counter must drop to zero and adopt
// the new deadline, while the cumulative total is untouched.
func TestResetCodexPrimaryWindowTokensZeroesCounterAndStampsDeadline(t *testing.T) {
	initStatsConfig(t)
	acct := config.Account{ID: "acc-reset", Enabled: true, AuthMethod: "codex"}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newStatsPool(acct)
	p.UpdateStats("acc-reset", 500, 0)

	newDeadline := time.Now().Add(5 * time.Hour).Unix()
	p.ResetCodexPrimaryWindowTokens("acc-reset", newDeadline)

	got := p.GetAllAccounts()[0]
	if got.CodexTokensSincePrimaryReset != 0 {
		t.Fatalf("window tokens = %d, want 0 after reset", got.CodexTokensSincePrimaryReset)
	}
	if got.CodexPrimaryResetAt != newDeadline {
		t.Fatalf("CodexPrimaryResetAt = %d, want %d", got.CodexPrimaryResetAt, newDeadline)
	}
	if got.TotalTokens != 500 {
		t.Fatalf("cumulative TotalTokens = %d, want 500 (reset must not clear it)", got.TotalTokens)
	}
}

// TestResetCodexPrimaryWindowTokensIgnoresUnknownAccount documents that the
// helper is a no-op for an untracked ID instead of creating a phantom entry.
func TestResetCodexPrimaryWindowTokensIgnoresUnknownAccount(t *testing.T) {
	p := newStatsPool()
	p.ResetCodexPrimaryWindowTokens("nobody", 123)
	if len(p.stats) != 0 {
		t.Fatalf("stats gained %d phantom entries", len(p.stats))
	}
}

// TestSyncCodexPrimaryWindowBootstrapsFromCumulativeTotal covers the account
// that joined mid-window before the per-window counter existed: bootstrap must
// lift the window total up to the cumulative total, and never lower it.
func TestSyncCodexPrimaryWindowBootstrapsFromCumulativeTotal(t *testing.T) {
	initStatsConfig(t)
	acct := config.Account{ID: "acc-sync", Enabled: true, AuthMethod: "codex"}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newStatsPool(acct)
	p.stats["acc-sync"] = &accountStats{TotalTokens: 900, CodexTokensSincePrimaryReset: 100}

	deadline := time.Now().Add(2 * time.Hour).Unix()
	p.SyncCodexPrimaryWindow("acc-sync", deadline, true)

	got := p.GetAllAccounts()[0]
	if got.CodexPrimaryResetAt != deadline {
		t.Fatalf("CodexPrimaryResetAt = %d, want %d", got.CodexPrimaryResetAt, deadline)
	}
	if got.CodexTokensSincePrimaryReset != 900 {
		t.Fatalf("window tokens = %d, want 900 bootstrapped from cumulative", got.CodexTokensSincePrimaryReset)
	}

	// bootstrapTokens=false must leave the counter alone even when it lags.
	p.stats["acc-sync"] = &accountStats{TotalTokens: 900, CodexTokensSincePrimaryReset: 100}
	p.SyncCodexPrimaryWindow("acc-sync", deadline, false)
	if got := p.GetAllAccounts()[0]; got.CodexTokensSincePrimaryReset != 100 {
		t.Fatalf("window tokens = %d, want 100 left untouched", got.CodexTokensSincePrimaryReset)
	}

	// Bootstrap must never reduce a window counter that already leads.
	p.stats["acc-sync"] = &accountStats{TotalTokens: 300, CodexTokensSincePrimaryReset: 800}
	p.SyncCodexPrimaryWindow("acc-sync", deadline, true)
	if got := p.GetAllAccounts()[0]; got.CodexTokensSincePrimaryReset != 800 {
		t.Fatalf("window tokens = %d, want 800 (bootstrap must not lower it)", got.CodexTokensSincePrimaryReset)
	}
}

// TestGetAllAccountsFullIncludesNonRoutableAccounts guards the Quota-page
// contract: every configured account is listed (including disabled ones that
// never enter the routing slice), with live stats overlaid where they exist.
func TestGetAllAccountsFullIncludesNonRoutableAccounts(t *testing.T) {
	initStatsConfig(t)
	routable := config.Account{ID: "acc-live", Enabled: true, AuthMethod: "social"}
	disabled := config.Account{ID: "acc-disabled", Enabled: false, AuthMethod: "social", TotalTokens: 42, RequestCount: 3}
	for _, a := range []config.Account{routable, disabled} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}
	p := newStatsPool(routable) // disabled account is not in the pool
	p.UpdateStats("acc-live", 10, 0)

	full := p.GetAllAccountsFull()
	if len(full) != 2 {
		t.Fatalf("GetAllAccountsFull len = %d, want 2", len(full))
	}
	byID := map[string]config.Account{}
	for _, a := range full {
		byID[a.ID] = a
	}
	if byID["acc-live"].TotalTokens != 10 {
		t.Fatalf("routable account tokens = %d, want live 10", byID["acc-live"].TotalTokens)
	}
	if byID["acc-disabled"].TotalTokens != 42 {
		t.Fatalf("disabled account tokens = %d, want persisted 42", byID["acc-disabled"].TotalTokens)
	}

	// GetAllAccounts (routable only) must not leak the disabled account.
	for _, a := range p.GetAllAccounts() {
		if a.ID == "acc-disabled" {
			t.Fatal("GetAllAccounts leaked a non-routable account")
		}
	}
}

// TestGetNextCodexSelectsOnlyCodexAccounts pins the capability selector used by
// image generation: non-codex accounts are never eligible, and the image model
// lock and exclusion set are both honoured.
func TestGetNextCodexSelectsOnlyCodexAccounts(t *testing.T) {
	initStatsConfig(t)
	p := newStatsPool(
		config.Account{ID: "social", Enabled: true, AuthMethod: "social"},
		config.Account{ID: "ext", Enabled: true, AuthMethod: "external_openai"},
		config.Account{ID: "codex-a", Enabled: true, AuthMethod: "codex"},
	)

	for i := 0; i < 8; i++ {
		acc := p.GetNextCodex(nil)
		if acc == nil {
			t.Fatalf("iteration %d: expected the codex account", i)
		}
		if acc.ID != "codex-a" {
			t.Fatalf("iteration %d: selected %q, want codex-a", i, acc.ID)
		}
	}

	if acc := p.GetNextCodex(map[string]bool{"codex-a": true}); acc != nil {
		t.Fatalf("excluded codex account was returned: %q", acc.ID)
	}

	p.modelLocks["codex-a"] = map[string]time.Time{"image": time.Now().Add(time.Hour)}
	if acc := p.GetNextCodex(nil); acc != nil {
		t.Fatalf("image-locked codex account was returned: %q", acc.ID)
	}
	delete(p.modelLocks, "codex-a")

	p.cooldowns["codex-a"] = time.Now().Add(time.Hour)
	if acc := p.GetNextCodex(nil); acc != nil {
		t.Fatalf("cooled-down codex account was returned: %q", acc.ID)
	}

	if acc := newStatsPool().GetNextCodex(nil); acc != nil {
		t.Fatalf("empty pool returned %q", acc.ID)
	}
}

// TestGetNextCodexReturnsCopy guards phase-01 C2 on the codex selector: the
// caller mutates token fields on what it gets back.
func TestGetNextCodexReturnsCopy(t *testing.T) {
	initStatsConfig(t)
	p := newStatsPool(config.Account{
		ID: "codex-a", Enabled: true, AuthMethod: "codex", AccessToken: "test-token-original",
	})
	acc := p.GetNextCodex(nil)
	if acc == nil {
		t.Fatal("expected the codex account")
	}
	if acc == &p.accounts[0] {
		t.Fatal("GetNextCodex returned a pointer into the pool slice")
	}
	acc.AccessToken = "test-token-mutated"
	if p.accounts[0].AccessToken != "test-token-original" {
		t.Fatalf("pool slice mutated through returned account: %q", p.accounts[0].AccessToken)
	}
}

// TestGetModelListRoundTripsCatalog covers the admin-facing catalog reader,
// including SetModelList's deliberate refusal to overwrite a known catalog with
// an empty one (a transient /v1/models failure must not blank the catalog).
func TestGetModelListRoundTripsCatalog(t *testing.T) {
	p := newStatsPool(config.Account{ID: "acc", Enabled: true})

	if got := p.GetModelList("acc"); len(got) != 0 {
		t.Fatalf("uncached account returned %v, want empty", got)
	}

	p.SetModelList("acc", []string{"Model-A", " model-b "})
	got := p.GetModelList("acc")
	if len(got) != 2 {
		t.Fatalf("GetModelList = %v, want 2 entries", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["model-a"] || !seen["model-b"] {
		t.Fatalf("catalog not normalised to trimmed lowercase: %v", got)
	}

	p.SetModelList("acc", nil)
	if got := p.GetModelList("acc"); len(got) != 2 {
		t.Fatalf("empty update blanked a known catalog: %v", got)
	}
}

// TestIsExternalAccountIDResolvesFromPoolSlice pins that the check reads the
// pool's own routing slice, so it works without config.Init.
func TestIsExternalAccountIDResolvesFromPoolSlice(t *testing.T) {
	p := newStatsPool(
		config.Account{ID: "ext", AuthMethod: "external_openai"},
		config.Account{ID: "router", AuthMethod: "agentrouter"},
		config.Account{ID: "router2", AuthMethod: "External_AgentRouter"},
		config.Account{ID: "codex", AuthMethod: "codex"},
	)
	for _, id := range []string{"ext", "router", "router2"} {
		if !p.isExternalAccountID(id) {
			t.Fatalf("%q should be external", id)
		}
	}
	if p.isExternalAccountID("codex") {
		t.Fatal("codex must not be classified external")
	}
	if p.isExternalAccountID("missing") {
		t.Fatal("unknown ID must not be classified external")
	}
	if p.isExternalAccountID("") {
		t.Fatal("empty ID must not be classified external")
	}
}

// TestIsContentBlockedError pins the payload-refusal classifier. Misclassifying
// this as an account fault would rotate the whole pool through a request that
// fails identically everywhere.
func TestIsContentBlockedError(t *testing.T) {
	blocked := []string{
		"upstream 400: content-blocked",
		"CONTENT_BLOCKER triggered",
		"request rejected: content blocked by policy",
	}
	for _, msg := range blocked {
		if !IsContentBlockedError(errors.New(msg)) {
			t.Fatalf("%q should be content-blocked", msg)
		}
	}
	notBlocked := []string{
		"HTTP 401 unauthorized",
		"HTTP 429 too many requests",
		"context deadline exceeded",
		"blocked account",
	}
	for _, msg := range notBlocked {
		if IsContentBlockedError(errors.New(msg)) {
			t.Fatalf("%q must not be classified content-blocked", msg)
		}
	}
	if IsContentBlockedError(nil) {
		t.Fatal("nil error must not be content-blocked")
	}
}
