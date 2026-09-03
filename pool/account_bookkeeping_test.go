package pool

import (
	"omniproxy/config"
	"testing"
	"time"
)

// PruneCacheWarmed must expire only entries older than the TTL. Pruning a live
// entry re-warms an account that is already warm, wasting tokens; keeping a
// stale one suppresses a warmup the upstream cache no longer has.
func TestPruneCacheWarmedDropsExpiredEntriesAndKeepsLiveOnes(t *testing.T) {
	p := newModelPool()
	const scope = "https://chatgpt.example"

	if !p.TryStartCacheWarm("acct", "gpt-5.6-sol", "stale-prefix", scope) {
		t.Fatal("first warmup should start")
	}
	p.CompleteCacheWarm("acct", "gpt-5.6-sol", "stale-prefix", scope)
	if !p.TryStartCacheWarm("acct", "gpt-5.6-sol", "fresh-prefix", scope) {
		t.Fatal("second warmup should start")
	}
	p.CompleteCacheWarm("acct", "gpt-5.6-sol", "fresh-prefix", scope)

	// Backdate only the stale entry past the 4-minute TTL.
	staleKey := cacheWarmedKey("acct", "gpt-5.6-sol", "stale-prefix")
	p.mu.Lock()
	p.cacheWarmedTS[staleKey] = time.Now().Add(-10 * time.Minute)
	p.mu.Unlock()

	p.PruneCacheWarmed()

	p.mu.RLock()
	_, staleKept := p.cacheWarmed[staleKey]
	_, freshKept := p.cacheWarmed[cacheWarmedKey("acct", "gpt-5.6-sol", "fresh-prefix")]
	p.mu.RUnlock()
	if staleKept {
		t.Error("expired warmed entry survived pruning")
	}
	if !freshKept {
		t.Error("live warmed entry was pruned")
	}

	// Pruning the stale entry must let that prefix be warmed again.
	if !p.TryStartCacheWarm("acct", "gpt-5.6-sol", "stale-prefix", scope) {
		t.Error("pruned prefix cannot be re-warmed")
	}
	if p.TryStartCacheWarm("acct", "gpt-5.6-sol", "fresh-prefix", scope) {
		t.Error("still-warm prefix was warmed again")
	}
}

// The scope-level circuit breaker must reopen once its retry window elapses,
// otherwise a single transient outage pauses warmups for the process lifetime.
func TestPruneCacheWarmedClearsElapsedRetryWindow(t *testing.T) {
	p := newModelPool()
	const scope = "https://chatgpt.example"
	key := cacheWarmScopeKey(scope)

	p.mu.Lock()
	p.cacheWarmRetryAfter[key] = time.Now().Add(-time.Second)
	p.cacheWarmRetryAfter[cacheWarmScopeKey("https://other.example")] = time.Now().Add(time.Hour)
	p.mu.Unlock()

	p.PruneCacheWarmed()

	p.mu.RLock()
	_, elapsedKept := p.cacheWarmRetryAfter[key]
	_, pendingKept := p.cacheWarmRetryAfter[cacheWarmScopeKey("https://other.example")]
	p.mu.RUnlock()
	if elapsedKept {
		t.Error("elapsed retry window was not cleared")
	}
	if !pendingKept {
		t.Error("a retry window still in the future was cleared early")
	}
}

func TestPruneCacheWarmedIsNoOpWhenTTLDisabled(t *testing.T) {
	p := newModelPool()
	p.cacheWarmedTTL = 0
	if !p.TryStartCacheWarm("acct", "gpt-5.6-sol", "prefix", "https://chatgpt.example") {
		t.Fatal("warmup should start")
	}
	p.CompleteCacheWarm("acct", "gpt-5.6-sol", "prefix", "https://chatgpt.example")

	key := cacheWarmedKey("acct", "gpt-5.6-sol", "prefix")
	p.mu.Lock()
	p.cacheWarmedTS[key] = time.Now().Add(-24 * time.Hour)
	p.mu.Unlock()

	p.PruneCacheWarmed()

	p.mu.RLock()
	_, kept := p.cacheWarmed[key]
	p.mu.RUnlock()
	if !kept {
		t.Error("TTL=0 must disable expiry, but the entry was pruned")
	}
}

// A refreshed token has to reach both routing slices. A media account lives only
// in serviceAccounts, so updating just p.accounts leaves it authenticating with
// the stale token until the next Reload.
func TestUpdateTokenUpdatesChatAndServiceSlices(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "chat", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1},
		config.Account{ID: "chat", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1},
	)
	p.serviceAccounts = []config.Account{{ID: "chat", AccessToken: "old-at", RefreshToken: "old-rt"}}

	p.UpdateToken("chat", "new-at", "new-rt", 999)

	p.mu.RLock()
	defer p.mu.RUnlock()
	// Every weighted duplicate must be updated, not only the first.
	for i := range p.accounts {
		got := p.accounts[i]
		if got.AccessToken != "new-at" || got.RefreshToken != "new-rt" || got.ExpiresAt != 999 {
			t.Fatalf("accounts[%d] = %+v, want the refreshed credentials", i, got)
		}
	}
	if got := p.serviceAccounts[0]; got.AccessToken != "new-at" || got.RefreshToken != "new-rt" {
		t.Fatalf("service account = %+v, want the refreshed credentials", got)
	}
}

// Google and OpenAI often omit refresh_token on refresh, meaning "keep the
// stored one". Writing the empty value would destroy the only way to refresh
// again — the account would need a full re-authentication.
func TestUpdateTokenKeepsStoredRefreshTokenWhenUpstreamOmitsIt(t *testing.T) {
	p := newModelPool(config.Account{ID: "acct", AccessToken: "old-at", RefreshToken: "keep-me"})

	p.UpdateToken("acct", "new-at", "", 500)

	p.mu.RLock()
	defer p.mu.RUnlock()
	if got := p.accounts[0]; got.RefreshToken != "keep-me" || got.AccessToken != "new-at" {
		t.Fatalf("account = %+v, want the stored refresh token preserved", got)
	}
}

// Count reports accounts, not weighted slots. A weight-3 account is three
// entries in the routing slice but one account to the operator.
func TestCountDeduplicatesWeightedEntries(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "heavy"},
		config.Account{ID: "heavy"},
		config.Account{ID: "heavy"},
		config.Account{ID: "light"},
	)
	if got := p.Count(); got != 2 {
		t.Fatalf("Count() = %d, want 2 unique accounts", got)
	}
}

// Stats live in p.stats keyed by account ID because Reload() rebuilds the
// accounts slice from the (staler) persisted config. GetAllAccounts must
// therefore overlay the in-memory counters rather than report the slice copy.
func TestGetAllAccountsOverlaysLiveStats(t *testing.T) {
	p := newModelPool(config.Account{ID: "acct", RequestCount: 1, TotalTokens: 10})
	p.mu.Lock()
	p.stats["acct"] = &accountStats{RequestCount: 42, TotalTokens: 4200, ErrorCount: 3, TotalCredits: 1.5, LastUsed: 777}
	p.mu.Unlock()

	all := p.GetAllAccounts()
	if len(all) != 1 {
		t.Fatalf("GetAllAccounts returned %d accounts, want 1", len(all))
	}
	got := all[0]
	if got.RequestCount != 42 || got.TotalTokens != 4200 || got.ErrorCount != 3 || got.LastUsed != 777 {
		t.Fatalf("account = %+v, want the live in-memory counters", got)
	}

	// The returned slice must be a copy: mutating it cannot corrupt routing.
	all[0].ID = "mutated"
	p.mu.RLock()
	stored := p.accounts[0].ID
	p.mu.RUnlock()
	if stored != "acct" {
		t.Fatalf("pool storage mutated through GetAllAccounts result: %q", stored)
	}
}

func TestResetStatsClearsOverlaidCounters(t *testing.T) {
	p := newModelPool(config.Account{ID: "acct"})
	p.mu.Lock()
	p.stats["acct"] = &accountStats{RequestCount: 9, TotalTokens: 900}
	p.mu.Unlock()

	p.ResetStats()

	if got := p.GetAllAccounts()[0]; got.RequestCount != 0 || got.TotalTokens != 0 {
		t.Fatalf("account after ResetStats = %+v, want zeroed counters", got)
	}
}

// The Codex per-window counter is separate from the cumulative total: a new
// window must start from zero even though lifetime usage keeps climbing.
func TestResetCodexPrimaryWindowTokensClearsOnlyTheWindowCounter(t *testing.T) {
	p := newModelPool(config.Account{ID: "codex", AuthMethod: "codex"})
	p.mu.Lock()
	p.stats["codex"] = &accountStats{TotalTokens: 5000, CodexTokensSincePrimaryReset: 800, CodexPrimaryResetAt: 1}
	p.mu.Unlock()

	p.ResetCodexPrimaryWindowTokens("codex", 12345)

	p.mu.RLock()
	stats := *p.stats["codex"]
	p.mu.RUnlock()
	if stats.CodexTokensSincePrimaryReset != 0 {
		t.Errorf("window counter = %d, want 0", stats.CodexTokensSincePrimaryReset)
	}
	if stats.CodexPrimaryResetAt != 12345 {
		t.Errorf("reset deadline = %d, want 12345", stats.CodexPrimaryResetAt)
	}
	if stats.TotalTokens != 5000 {
		t.Errorf("cumulative total = %d, want it left untouched at 5000", stats.TotalTokens)
	}
}

// An account added mid-window has no window counter, so its usage is invisible
// until the next reset. Bootstrapping seeds it from the cumulative total; it
// must never lower a counter that is already ahead.
func TestSyncCodexPrimaryWindowBootstrapsOnlyUpward(t *testing.T) {
	p := newModelPool(config.Account{ID: "codex", AuthMethod: "codex"})
	p.mu.Lock()
	p.stats["codex"] = &accountStats{TotalTokens: 700, CodexTokensSincePrimaryReset: 100}
	p.mu.Unlock()

	p.SyncCodexPrimaryWindow("codex", 555, true)
	p.mu.RLock()
	bootstrapped := *p.stats["codex"]
	p.mu.RUnlock()
	if bootstrapped.CodexTokensSincePrimaryReset != 700 || bootstrapped.CodexPrimaryResetAt != 555 {
		t.Fatalf("bootstrapped stats = %+v, want window seeded to 700 and deadline 555", bootstrapped)
	}

	// Without the bootstrap flag only the deadline moves.
	p.SyncCodexPrimaryWindow("codex", 666, false)
	p.mu.RLock()
	synced := *p.stats["codex"]
	p.mu.RUnlock()
	if synced.CodexTokensSincePrimaryReset != 700 || synced.CodexPrimaryResetAt != 666 {
		t.Fatalf("synced stats = %+v, want the counter unchanged and deadline 666", synced)
	}
}

func TestGetModelListReturnsCachedCatalogAndEmptyWhenUnknown(t *testing.T) {
	p := newModelPool(config.Account{ID: "acct"})
	if got := p.GetModelList("acct"); len(got) != 0 {
		t.Fatalf("uncached catalog = %v, want empty", got)
	}

	p.SetModelList("acct", []string{"Claude-Opus-5", " gpt-5.6-sol "})
	got := p.GetModelList("acct")
	if len(got) != 2 {
		t.Fatalf("catalog = %v, want 2 entries", got)
	}
	// IDs are normalised to lower-case and trimmed on write.
	seen := map[string]bool{got[0]: true, got[1]: true}
	for _, want := range []string{"claude-opus-5", "gpt-5.6-sol"} {
		if !seen[want] {
			t.Errorf("catalog %v missing normalised id %q", got, want)
		}
	}

	// An empty response means "unknown", not "supports nothing": it must not
	// erase a catalog that was already discovered.
	p.SetModelList("acct", nil)
	if got := p.GetModelList("acct"); len(got) != 2 {
		t.Fatalf("catalog after empty update = %v, want the previous 2 entries", got)
	}
}
