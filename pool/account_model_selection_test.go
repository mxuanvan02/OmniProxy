package pool

import (
	"omniproxy/config"
	"path/filepath"
	"testing"
	"time"
)

// newModelPool builds a pool with every runtime map initialised. RecordSuccess,
// RecordError and the cache-warm bookkeeping all write into these maps, so a
// zero-value AccountPool panics.
func newModelPool(accounts ...config.Account) *AccountPool {
	return &AccountPool{
		accounts:              accounts,
		cooldowns:             make(map[string]time.Time),
		errorCounts:           make(map[string]int),
		modelLists:            make(map[string]map[string]bool),
		modelLocks:            make(map[string]map[string]time.Time),
		stats:                 make(map[string]*accountStats),
		cacheWarmed:           make(map[string]bool),
		cacheWarmedTS:         make(map[string]time.Time),
		cacheWarmedTTL:        4 * time.Minute,
		cacheWarming:          make(map[string]bool),
		cacheWarmFailureCount: make(map[string]int),
		cacheWarmRetryAfter:   make(map[string]time.Time),
	}
}

// initTempPoolConfig gives the test its own config file and restores a clean one
// afterwards, so allowOverUsage cannot leak into a later test in this package.
func initTempPoolConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(dir, "reset.json")) })
}

func TestGetNextForModelEmptyPoolYieldsNoAccount(t *testing.T) {
	initTempPoolConfig(t)
	if got := newModelPool().GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("empty pool returned %#v, want nil", got)
	}
}

// Every account cooled down must yield nil rather than the soonest-recovering
// one: handing back a still-cooling account sends the request straight into the
// failure that caused the cooldown.
func TestGetNextForModelAllCooledDownYieldsNoAccount(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	p.mu.Lock()
	p.cooldowns["a"] = time.Now().Add(10 * time.Minute)
	p.cooldowns["b"] = time.Now().Add(time.Minute)
	p.mu.Unlock()

	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("all-cooled pool returned %#v, want nil", got)
	}
}

// A known catalog is authoritative: only the subset that lists the model may be
// selected, however many times selection is repeated.
func TestGetNextForModelSelectsOnlyTheSupportingSubset(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(
		config.Account{ID: "supports"},
		config.Account{ID: "lacks-1"},
		config.Account{ID: "lacks-2"},
	)
	p.SetModelList("supports", []string{"claude-opus-5"})
	p.SetModelList("lacks-1", []string{"gpt-5.6-sol"})
	p.SetModelList("lacks-2", []string{"claude-sonnet-5"})

	for i := 0; i < 6; i++ {
		got := p.GetNextForModel("claude-opus-5")
		if got == nil {
			t.Fatalf("iteration %d: no account for a model one account serves", i)
		}
		if got.ID != "supports" {
			t.Fatalf("iteration %d selected %q, want the only catalog match", i, got.ID)
		}
	}
}

// The per-account upstream Overages switch keeps an over-quota account routable
// without the global allowOverUsage setting.
func TestGetNextForModelHonoursUpstreamOverageSwitch(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(config.Account{
		ID: "overage-on", UsageCurrent: 10, UsageLimit: 10, OverageStatus: "ENABLED",
	})

	got := p.GetNextForModel("some-model")
	if got == nil || got.ID != "overage-on" {
		t.Fatalf("upstream-enabled overage account not selected: %#v", got)
	}
}

func TestCountAccountsForModelCountsOnlySupportingAccounts(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "opus-1"},
		config.Account{ID: "opus-2"},
		config.Account{ID: "sonnet-only"},
		config.Account{ID: "no-catalog"},
	)
	p.SetModelList("opus-1", []string{"claude-opus-5"})
	p.SetModelList("opus-2", []string{"claude-opus-5"})
	p.SetModelList("sonnet-only", []string{"claude-sonnet-5"})

	// opus-1, opus-2 and the uncatalogued account are all eligible; the account
	// with a known catalog that lacks the model is not.
	if got := p.CountAccountsForModel("claude-opus-5"); got != 3 {
		t.Fatalf("CountAccountsForModel(claude-opus-5) = %d, want 3", got)
	}
	if got := p.CountAccountsForModel("claude-sonnet-5"); got != 2 {
		t.Fatalf("CountAccountsForModel(claude-sonnet-5) = %d, want 2", got)
	}
}

func TestGetByIDFindsChatAndServiceOnlyAccounts(t *testing.T) {
	p := newModelPool(config.Account{ID: "chat-account", AccessToken: "chat-token"})
	p.serviceAccounts = []config.Account{{ID: "image-only", Capabilities: []string{"image"}}}

	chat := p.GetByID("chat-account")
	if chat == nil || chat.AccessToken != "chat-token" {
		t.Fatalf("GetByID(chat-account) = %#v", chat)
	}
	// A media account never enters p.accounts; it must still resolve by ID.
	if svc := p.GetByID("image-only"); svc == nil || svc.ID != "image-only" {
		t.Fatalf("GetByID(image-only) = %#v, want the service account", svc)
	}
	if missing := p.GetByID("nonexistent"); missing != nil {
		t.Fatalf("GetByID(nonexistent) = %#v, want nil", missing)
	}

	// GetByID must hand back a copy, not a pointer into pool storage.
	chat.AccessToken = "mutated"
	if again := p.GetByID("chat-account"); again == nil || again.AccessToken != "chat-token" {
		t.Fatalf("pool storage mutated through GetByID result: %#v", again)
	}
}

// Quota exhaustion blocks selection by default and is overridden by the global
// allowOverUsage setting. Both directions matter: the first keeps a dead account
// out of rotation, the second is the operator's explicit escape hatch.
func TestGetNextForModelQuotaExhaustionRespectsAllowOverUsage(t *testing.T) {
	initTempPoolConfig(t)
	exhausted := config.Account{ID: "exhausted", UsageCurrent: 10, UsageLimit: 10}

	p := newModelPool(exhausted)
	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("quota-exhausted account was selected: %#v", got)
	}

	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}
	if got := p.GetNextForModel("claude-opus-5"); got == nil || got.ID != "exhausted" {
		t.Fatalf("allowOverUsage=true did not re-admit the account: %#v", got)
	}
}

// An external provider whose credits are spent is unusable regardless of the
// Kiro-shaped usage fields, so it must be filtered on the credit signal alone.
func TestGetNextForModelSkipsExternalAccountWithSpentCredits(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(
		config.Account{ID: "spent", AuthMethod: "external_openai", ExtCreditLimit: 100, ExtCreditsUsed: 100},
		config.Account{ID: "flagged", AuthMethod: "external_openai", ExtStatus: "exhausted"},
	)

	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("credit-exhausted external account was selected: %#v", got)
	}
}

// One healthy account among cooled-down and quota-blocked peers must still be
// found — this is the failover path that keeps requests served.
func TestGetNextForModelFindsTheOneHealthyAccount(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(
		config.Account{ID: "cooled"},
		config.Account{ID: "exhausted", UsageCurrent: 5, UsageLimit: 5},
		config.Account{ID: "healthy"},
		config.Account{ID: "model-locked"},
	)
	p.mu.Lock()
	p.cooldowns["cooled"] = time.Now().Add(time.Hour)
	p.modelLocks["model-locked"] = map[string]time.Time{"claude-opus-5": time.Now().Add(time.Hour)}
	p.mu.Unlock()

	for i := 0; i < 8; i++ {
		got := p.GetNextForModel("claude-opus-5")
		if got == nil || got.ID != "healthy" {
			t.Fatalf("iteration %d selected %#v, want the sole healthy account", i, got)
		}
	}
}

// Selectors hand back a copy. A caller mutating token fields on the result (which
// the refresh path does) must not write through into pool storage, where it would
// race other selectors and be orphaned by Reload replacing the slice.
func TestGetNextForModelReturnsCopyNotPoolStorage(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(config.Account{ID: "acct", AccessToken: "original-token"})

	first := p.GetNextForModel("claude-opus-5")
	if first == nil {
		t.Fatal("expected an account")
	}
	first.AccessToken = "mutated-by-caller"

	second := p.GetNextForModel("claude-opus-5")
	if second == nil {
		t.Fatal("expected an account on the second call")
	}
	if second.AccessToken != "original-token" {
		t.Fatalf("pool storage was mutated through the returned pointer: %q", second.AccessToken)
	}
	if first == second {
		t.Fatal("two selections returned the same pointer, want independent copies")
	}
}

// The preferred-provider phase (claude-* prefers external accounts) is a separate
// return path in the selector and must copy too.
func TestGetNextForModelPreferredPhaseAlsoReturnsCopy(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(config.Account{
		ID: "external", AuthMethod: "external_openai", AccessToken: "external-token",
	})
	p.SetModelList("external", []string{"claude-opus-5"})

	first := p.GetNextForModel("claude-opus-5")
	if first == nil || first.ID != "external" {
		t.Fatalf("preferred external account not selected: %#v", first)
	}
	first.AccessToken = "mutated-by-caller"

	if second := p.GetNextForModel("claude-opus-5"); second == nil || second.AccessToken != "external-token" {
		t.Fatalf("preferred-phase result aliased pool storage: %#v", second)
	}
}
