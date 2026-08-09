package pool

import (
	"errors"
	"fmt"
	"omniproxy/config"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		`HTTP 403 from 10k: {"error":{"type":"rate_limit_error"}}`,
		"HTTP 403: too many requests",
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsTransientError
// ---------------------------------------------------------------------------

func TestIsTransientErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"HTTP 503 from bddevlab: {\"error\":{\"message\":\"system cpu overloaded (current: 100.0%, threshold: 90%)\"}}",
		"HTTP 502 Bad Gateway",
		"HTTP 504 Gateway Timeout",
		"context deadline exceeded",
		"connection reset by peer",
		"service unavailable",
		"EOF",
		"no such host",
	}
	for _, msg := range positives {
		if !IsTransientError(errors.New(msg)) {
			t.Errorf("IsTransientError(%q) = false, want true", msg)
		}
	}
}

func TestIsTransientErrorIgnoresNonTransient(t *testing.T) {
	negatives := []string{
		"HTTP 401 Unauthorized",
		"HTTP 403 Forbidden",
		`HTTP 403 from 10k: {"error":{"type":"rate_limit_error"}}`,
		"HTTP 429: too many requests",
		"stream idle timeout: upstream produced no data within idle window",
		"net/http: timeout awaiting response headers",
		"invalid_grant",
		"some unrelated error",
		"model_not_found",
	}
	for _, msg := range negatives {
		if IsTransientError(errors.New(msg)) {
			t.Errorf("IsTransientError(%q) = true, want false", msg)
		}
	}
}

func TestIsTransientErrorNilError(t *testing.T) {
	if IsTransientError(nil) {
		t.Fatal("IsTransientError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelPrefersAgentRouterForClaude(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "kiro", AuthMethod: "social"},
		config.Account{ID: "agentrouter", AuthMethod: "agentrouter"},
		config.Account{ID: "openai", AuthMethod: "external_openai"},
	)

	got := p.GetNextForModelExcluding("claude-opus-5", nil)
	if got == nil {
		t.Fatal("expected a Claude-capable external account")
	}
	if got.AuthMethod != "agentrouter" && got.AuthMethod != "external_openai" && got.AuthMethod != "external_agentrouter" {
		t.Fatalf("selected %q (%s), want an external provider", got.ID, got.AuthMethod)
	}
}

func TestAgentRouterCatalogFiltersUnsupportedModel(t *testing.T) {
	p := newTestPool(config.Account{ID: "agentrouter", AuthMethod: "agentrouter"})
	p.SetModelList("agentrouter", []string{"gpt-only-model"})

	if got := p.GetNextForModelExcluding("claude-opus-5", nil); got != nil {
		t.Fatalf("AgentRouter catalog does not contain claude-opus-5, got %#v", got)
	}
}

func TestExternalCatalogMatchesClaudeAliasToSnapshot(t *testing.T) {
	p := newTestPool(config.Account{ID: "external", AuthMethod: "external_openai"})
	p.SetModelList("external", []string{"claude-opus-5-20260801"})

	got := p.GetNextForModelExcluding("claude-opus-5[1m]", nil)
	if got == nil || got.ID != "external" {
		t.Fatalf("expected Claude alias to match dated snapshot, got %#v", got)
	}
}

func TestGetNextForModelExhaustsAllMatchingAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "match-1"},
		config.Account{ID: "match-2"},
		config.Account{ID: "match-3"},
		config.Account{ID: "match-4"},
		config.Account{ID: "match-5"},
		config.Account{ID: "other-model"},
	)
	for i := 1; i <= 5; i++ {
		p.SetModelList(fmt.Sprintf("match-%d", i), []string{"claude-opus-5"})
	}
	p.SetModelList("other-model", []string{"claude-sonnet-5"})

	excluded := make(map[string]bool)
	for len(excluded) < 5 {
		account := p.GetNextForModelExcluding("claude-opus-5", excluded)
		if account == nil {
			t.Fatalf("routing stopped after %d matching accounts", len(excluded))
		}
		if account.ID == "other-model" {
			t.Fatal("selected an account whose catalog lacks claude-opus-5")
		}
		excluded[account.ID] = true
	}
	if account := p.GetNextForModelExcluding("claude-opus-5", excluded); account != nil {
		t.Fatalf("expected matching account set to be exhausted, got %#v", account)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

// ---------------------------------------------------------------------------
// Model cooldown filtering
// ---------------------------------------------------------------------------

func TestGetNextForModelSkipsModelLockedAccount(t *testing.T) {
	p := &AccountPool{
		accounts:    []config.Account{{ID: "only", Enabled: true}},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
		modelLocks:  make(map[string]map[string]time.Time),
	}
	// Arm a model lock 3 errors deep so RecordError actually sets it.
	for i := 0; i < 3; i++ {
		p.RecordError("only", false, "claude-sonnet-4.5")
	}

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", nil)
	if acc != nil {
		t.Fatalf("expected model-locked account to be skipped, got %#v", acc)
	}
}

func TestGetNextForModelDoesNotReuseSoonestUnlockingAccount(t *testing.T) {
	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "late", Enabled: true},
			{ID: "soon", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
		modelLocks: map[string]map[string]time.Time{
			"late": {"m": now.Add(10 * time.Minute)},
			"soon": {"m": now.Add(1 * time.Minute)},
		},
	}

	acc := p.GetNextForModelExcluding("m", nil)
	if acc != nil {
		t.Fatalf("expected all model-locked accounts to be skipped, got %#v", acc)
	}
}

// TestGetNextForModelStillExcludesExplicitlyExcluded verifies the fallback does
// not resurrect an account the caller explicitly excluded (already-tried this
// request), even when it would otherwise be the soonest-unlocking candidate.
func TestGetNextForModelStillExcludesExplicitlyExcluded(t *testing.T) {
	now := time.Now()
	p := &AccountPool{
		accounts:    []config.Account{{ID: "only", Enabled: true}},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
		modelLocks:  map[string]map[string]time.Time{"only": {"m": now.Add(time.Minute)}},
	}

	if acc := p.GetNextForModelExcluding("m", map[string]bool{"only": true}); acc != nil {
		t.Fatalf("expected nil when the only account is excluded, got %q", acc.ID)
	}
}

// ---------------------------------------------------------------------------
// Prompt-cache routing and warmup control
// ---------------------------------------------------------------------------

func TestCacheWarmStateIsModelScoped(t *testing.T) {
	p := newTestPool()
	const (
		accountID = "acct"
		prefixKey = "prefix"
		scope     = "https://chatgpt.example"
	)

	if !p.TryStartCacheWarm(accountID, "gpt-5.6-sol", prefixKey, scope) {
		t.Fatal("first sol warmup should start")
	}
	p.CompleteCacheWarm(accountID, "gpt-5.6-sol", prefixKey, scope)

	if p.TryStartCacheWarm(accountID, "gpt-5.6-sol", prefixKey, scope) {
		t.Fatal("warm sol prefix should not start again before its TTL expires")
	}
	if !p.TryStartCacheWarm(accountID, "gpt-5.6-terra", prefixKey, scope) {
		t.Fatal("warming sol must not suppress the same prefix for terra")
	}
}

func TestTryStartCacheWarmDeduplicatesConcurrentRequests(t *testing.T) {
	p := newTestPool()
	var started int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.TryStartCacheWarm("acct", "gpt-5.6-sol", "prefix", "https://chatgpt.example") {
				atomic.AddInt32(&started, 1)
			}
		}()
	}
	wg.Wait()

	if started != 1 {
		t.Fatalf("concurrent warmup reservations: got %d, want 1", started)
	}
}

func TestCacheWarmFailureBackoffIsSharedByUpstreamScope(t *testing.T) {
	p := newTestPool()
	const scope = "https://chatgpt.example"
	if !p.TryStartCacheWarm("a", "gpt-5.6-sol", "prefix-a", scope) {
		t.Fatal("first warmup should start")
	}
	p.FailCacheWarm("a", "gpt-5.6-sol", "prefix-a", scope)

	p.mu.RLock()
	firstRetry := p.cacheWarmRetryAfter[cacheWarmScopeKey(scope)]
	p.mu.RUnlock()
	if remaining := time.Until(firstRetry); remaining < 29*time.Second || remaining > 31*time.Second {
		t.Fatalf("first retry delay: got %s, want about 30s", remaining)
	}
	if p.TryStartCacheWarm("b", "gpt-5.6-terra", "prefix-b", scope) {
		t.Fatal("a failed endpoint must pause warmups for every account sharing it")
	}

	// Advance only the circuit-breaker clock so a second failure can be observed.
	p.mu.Lock()
	p.cacheWarmRetryAfter[cacheWarmScopeKey(scope)] = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if !p.TryStartCacheWarm("b", "gpt-5.6-terra", "prefix-b", scope) {
		t.Fatal("warmup should resume after the retry window")
	}
	p.FailCacheWarm("b", "gpt-5.6-terra", "prefix-b", scope)

	p.mu.RLock()
	secondRetry := p.cacheWarmRetryAfter[cacheWarmScopeKey(scope)]
	secondCount := p.cacheWarmFailureCount[cacheWarmScopeKey(scope)]
	p.mu.RUnlock()
	if secondCount != 2 {
		t.Fatalf("failure count: got %d, want 2", secondCount)
	}
	if remaining := time.Until(secondRetry); remaining < 59*time.Second || remaining > 61*time.Second {
		t.Fatalf("second retry delay: got %s, want about 1m", remaining)
	}
}

func TestCacheStickinessIsModelScoped(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "sol-account", Enabled: true},
		config.Account{ID: "terra-account", Enabled: true},
	)
	p.RecordCacheStickiness("gpt-5.6-sol", "prefix", "sol-account")
	p.RecordCacheStickiness("gpt-5.6-terra", "prefix", "terra-account")

	if got := p.GetNextForModelWithCacheKey("gpt-5.6-sol", nil, "prefix"); got == nil || got.ID != "sol-account" {
		t.Fatalf("sol sticky account: got %#v", got)
	}
	if got := p.GetNextForModelWithCacheKey("gpt-5.6-terra", nil, "prefix"); got == nil || got.ID != "terra-account" {
		t.Fatalf("terra sticky account: got %#v", got)
	}
}
