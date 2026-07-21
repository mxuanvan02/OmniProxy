// Package pool manages account pools
// Implements round-robin load balancing, error cooldown, token refresh
package pool

import (
	"omniproxy/config"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

// accountStats holds the cumulative runtime counters for a single account.
// It lives in AccountPool.stats keyed by accountID, deliberately separate from
// the accounts[] routing slice: Reload() rebuilds accounts[] from config on
// every account mutation (40+ call sites), so counters stored on the slice were
// silently reset by racing reloads. Keeping them here means routing rebuilds no
// longer clobber usage totals.
type accountStats struct {
	RequestCount int
	ErrorCount   int
	TotalTokens  int
	TotalCredits float64
	LastUsed     int64
}

// AccountPool manages the account pool
type AccountPool struct {
	mu            sync.RWMutex
	accounts      []config.Account
	totalAccounts int
	currentIndex  uint64
	cooldowns     map[string]time.Time       // account cooldown time
	errorCounts   map[string]int             // consecutive error count
	modelLists    map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	modelLocks    map[string]map[string]time.Time // accountID → modelName → cooldown until
	stats         map[string]*accountStats   // accountID → cumulative runtime stats (survives Reload)
	// cacheSticky maps a prompt-cache key (derived from the conversation ID)
	// to the account ID that last handled it. Used to pin consecutive turns
	// from the same conversation to the same upstream account so the
	// provider's prompt cache can warm up and serve hits.
	cacheSticky    map[string]string   // cacheKey → accountID
	cacheStickyTS  map[string]time.Time // cacheKey → last seen time
	cacheStickyTTL time.Duration
	// cacheWarmed tracks (accountID + cacheKey) pairs that have already been
	// warmed via a background warmup request. Used by on-rotation warming to
	// avoid re-warming the same account for the same cache key (which would
	// waste tokens and rate-limit budget).
	cacheWarmed   map[string]bool      // "accountID|cacheKey" → true
	cacheWarmedTS map[string]time.Time // "accountID|cacheKey" → warm time
	cacheWarmedTTL time.Duration       // expire warmed entries after 1h
	// cacheWarming tracks (accountID + cacheKey) pairs that currently have
	// a warmup request in flight. Prevents duplicate warmups from concurrent
	// requests to the same account+cacheKey.
	cacheWarming map[string]bool // "accountID|cacheKey" → true
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool returns the global account pool singleton
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:      make(map[string]time.Time),
			errorCounts:    make(map[string]int),
			modelLists:     make(map[string]map[string]bool),
			modelLocks:     make(map[string]map[string]time.Time),
			stats:          make(map[string]*accountStats),
			cacheSticky:    make(map[string]string),
			cacheStickyTS:  make(map[string]time.Time),
			cacheStickyTTL: 30 * time.Minute, // expire pinning 30 min after last use
			cacheWarmed:    make(map[string]bool),
			cacheWarmedTS:  make(map[string]time.Time),
			cacheWarmedTTL: 1 * time.Hour, // warmed entries expire after 1h
			cacheWarming:   make(map[string]bool),
		}
		pool.Reload()
	})
	return pool
}

// Reload rebuilds the weighted account list from config.
// Weight <= 1 → 1 entry; weight >= 2 → weight entries.
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	var weighted []config.Account
	for _, a := range enabled {
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		w := effectiveWeight(a.Weight)
		for j := 0; j < w; j++ {
			weighted = append(weighted, a)
		}
	}
	p.accounts = weighted
	p.totalAccounts = len(enabled)

	// Seed runtime stats from the persisted config counters, but only for
	// accounts we are not already tracking in memory. The in-memory map is the
	// source of truth once the process is running (it accumulates every request
	// and is flushed to config asynchronously); overwriting it here would undo
	// increments that raced ahead of the last flush — the original under-count
	// bug. New accounts (or a fresh process) legitimately start from config.
	if p.stats == nil {
		p.stats = make(map[string]*accountStats)
	}
	for _, a := range enabled {
		if _, ok := p.stats[a.ID]; ok {
			continue
		}
		p.stats[a.ID] = &accountStats{
			RequestCount: a.RequestCount,
			ErrorCount:   a.ErrorCount,
			TotalTokens:  a.TotalTokens,
			TotalCredits: a.TotalCredits,
			LastUsed:     a.LastUsed,
		}
	}
}

// GetNext returns the next available account (weighted round-robin)
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding returns the next available account, skipping specified ones.
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)

	// weighted round-robin: find available account
	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if excluded != nil && excluded[acc.ID] {
			seen[acc.ID] = true
			continue
		}
		if seen[acc.ID] {
			continue
		}

		// skip accounts in cooldown
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			continue
		}

		// Skip accounts whose quota is exhausted, unless overrides apply.
		if isQuotaBlocked(*acc, allowOverUsage) {
			seen[acc.ID] = true
			continue
		}

		return acc
	}

		// no available accounts, return the one with shortest cooldown (exclude exhausted unless overage allowed)
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

// SetModelList caches the model set for an account (called by handler after refresh)
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList returns cached model IDs for the account (for admin API).
// Returns empty slice if not yet cached.
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel checks if the account supports the given model.
// If the account has no model list (cold start), assume all models supported.
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	// External OpenAI-compatible providers route model selection to the
	// upstream provider, which owns its own model registry. Always allow so
	// external accounts are never skipped because of a stale/empty cache.
	if p.isExternalAccountID(accountID) {
		return true
	}
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // cold start: list not ready, optimistically allow
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// isExternalAccountID reports whether the account with the given ID is an
// external OpenAI-compatible provider. It looks up the auth method from the
// pool's own routing slice (already populated from config during Reload) so it
// never depends on the global config singleton being initialized — tests
// construct pools directly without calling config.Init.
func (p *AccountPool) isExternalAccountID(accountID string) bool {
	if accountID == "" {
		return false
	}
	for i := range p.accounts {
		if p.accounts[i].ID == accountID {
			return p.accounts[i].AuthMethod == "external_openai"
		}
	}
	return false
}

// GetNextForModel returns the next available account supporting the given model.
// model should be the actual model name with thinking suffix removed.
// If no account has model list data, behaves like GetNext (optimistic routing).
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding returns the next account supporting the model, skipping specified ones.
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)

	// Strategy mode: when cost-optimized or reset-aware is configured AND the
	// pool has >= strategyMinPoolSize unique accounts, we collect all filter-
	// passing candidates and pick the best one by score instead of returning
	// the first round-robin hit. This avoids wasting requests on accounts
	// near quota exhaustion / reset. Round-robin (default) keeps the original
	// early-return behaviour for zero overhead.
	useStrategy := p.strategyShouldApply()
	var preferredCandidates []config.Account
	var allCandidates []config.Account

	// Provider-aware routing: when the requested model is a Claude model
	// (claude-*), prefer External OpenAI-compatible accounts that actually
	// serve it. Codex accounts do not serve Claude models — without this
	// preference the round-robin would frequently land on a Codex account,
	// which then silently substitutes gpt-5.6-* for the Claude request.
	// Conversely, gpt-* models should prefer Codex accounts.
	preferExternal := strings.HasPrefix(strings.ToLower(model), "claude")
	preferCodex := strings.HasPrefix(strings.ToLower(model), "gpt-")

	// tryPass returns the account if it passes all filters, else nil.
	// Filters: excluded, seen, accountHasModel, isModelLocked, cooldown, isQuotaBlocked.
	tryPass := func(acc *config.Account) *config.Account {
		if excluded != nil && excluded[acc.ID] {
			seen[acc.ID] = true
			return nil
		}
		if seen[acc.ID] {
			return nil
		}
		if !p.accountHasModel(acc.ID, model) {
			seen[acc.ID] = true
			return nil
		}
		if p.isModelLocked(acc.ID, model, now) {
			seen[acc.ID] = true
			return nil
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			return nil
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			seen[acc.ID] = true
			return nil
		}
		return acc
	}

	// Phase 1: preferred-provider pass. Iterate the weighted slice starting
	// from the atomic cursor and return the first preferred account that
	// passes all filters. This gives preferred accounts priority while still
	// round-robining among them.
	if preferExternal || preferCodex {
		isPreferred := func(acc *config.Account) bool {
			if preferExternal {
				return acc.AuthMethod == "external_openai"
			}
			// preferCodex
			return acc.AuthMethod == "codex"
		}
		for i := 0; i < n; i++ {
			idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
			acc := &p.accounts[idx]
			if !isPreferred(acc) {
				continue
			}
			if picked := tryPass(acc); picked != nil {
				if useStrategy {
					preferredCandidates = append(preferredCandidates, *picked)
					continue
				}
				return picked
			}
		}
		// Strategy mode: if we collected preferred candidates, pick the best
		// one by score and return it without falling through to Phase 2.
		if useStrategy && len(preferredCandidates) > 0 {
			return pickByStrategy(preferredCandidates, now)
		}
	}

	// Phase 2: standard round-robin over every account (preferred + others).
	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if excluded != nil && excluded[acc.ID] {
			seen[acc.ID] = true
			continue
		}
		if seen[acc.ID] {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			seen[acc.ID] = true
			continue
		}
		if p.isModelLocked(acc.ID, model, now) {
			seen[acc.ID] = true
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			seen[acc.ID] = true
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			seen[acc.ID] = true
			continue
		}
		if useStrategy {
			allCandidates = append(allCandidates, *acc)
			continue
		}
		return acc
	}

	// Strategy mode: pick the best-scoring candidate from Phase 2.
	if useStrategy && len(allCandidates) > 0 {
		return pickByStrategy(allCandidates, now)
	}

	// Fallback: no immediately-available account. Return the enabled account that
	// becomes available soonest (shortest account cooldown OR model lock), so the
	// request attempts a real upstream call instead of surfacing a misleading 503
	// "No available accounts". A purely in-memory model lock must NOT make an
	// otherwise-usable account vanish — that is what forced an operator restart.
	// Only quota-blocked accounts (a persisted, intentional state) stay excluded.
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		// Soonest time this account is free of every in-memory penalty.
		var until time.Time
		if cd, ok := p.cooldowns[acc.ID]; ok && cd.After(until) {
			until = cd
		}
		if locks, ok := p.modelLocks[acc.ID]; ok && model != "" {
			if ml, ok := locks[model]; ok && ml.After(until) {
				until = ml
			}
		}
		if until.IsZero() {
			// No penalty at all — usable right now.
			return acc
		}
		if best == nil || until.Before(earliest) {
			best = acc
			earliest = until
		}
	}
	return best
}

// GetNextForModelWithCacheKey works like GetNextForModelExcluding but first
// tries the account that last handled the same cacheKey. This keeps
// consecutive turns from the same conversation on the same upstream account
// so the provider's prompt cache can warm up and serve hits.
//
// cacheKey is the opaque hash derived from the conversation ID (see
// codexCacheKey). When cacheKey is empty, falls back to normal rotation.
func (p *AccountPool) GetNextForModelWithCacheKey(model string, excluded map[string]bool, cacheKey string) *config.Account {
	if cacheKey != "" {
		p.mu.RLock()
		stickyID, ok := p.cacheSticky[cacheKey]
		p.mu.RUnlock()
		if ok && stickyID != "" {
			// Try the sticky account first — but only if it's not excluded,
			// not in cooldown, not quota-blocked, and supports the model.
			if excluded == nil || !excluded[stickyID] {
				if acc := p.getAccountIfAvailable(stickyID, model); acc != nil {
					return acc
				}
			}
		}
	}
	return p.GetNextForModelExcluding(model, excluded)
}

// getAccountIfAvailable returns the account by ID if it is enabled, supports
// the model, is not in cooldown, not model-locked, and not quota-blocked.
// Returns nil otherwise. Caller must hold no pool lock.
func (p *AccountPool) getAccountIfAvailable(accountID, model string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	for i := range p.accounts {
		acc := &p.accounts[i]
		if acc.ID != accountID {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			return nil
		}
		if p.isModelLocked(acc.ID, model, now) {
			return nil
		}
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd) {
			return nil
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			return nil
		}
		return acc
	}
	return nil
}

// RecordCacheStickiness pins cacheKey → accountID so subsequent requests
// with the same cacheKey prefer this account. Called after a successful
// upstream response.
func (p *AccountPool) RecordCacheStickiness(cacheKey, accountID string) {
	if cacheKey == "" || accountID == "" {
		return
	}
	p.mu.Lock()
	p.cacheSticky[cacheKey] = accountID
	p.cacheStickyTS[cacheKey] = time.Now()
	p.mu.Unlock()
}

// PruneCacheSticky removes cache-sticky entries older than the TTL.
// Called periodically from the background refresh loop.
func (p *AccountPool) PruneCacheSticky() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cacheStickyTTL <= 0 {
		return
	}
	cutoff := time.Now().Add(-p.cacheStickyTTL)
	for key, ts := range p.cacheStickyTS {
		if ts.Before(cutoff) {
			delete(p.cacheSticky, key)
			delete(p.cacheStickyTS, key)
		}
	}
}

// cacheWarmedKey builds the composite key for the warmed registry.
func cacheWarmedKey(accountID, cacheKey string) string {
	return accountID + "|" + cacheKey
}

// IsCacheWarmed reports whether accountID has already been warmed for the
// given cacheKey (i.e. a warmup request with the same instructions prefix
// has been sent to this account). Used by on-rotation warming to skip the
// warmup step when the account already has a hot cache entry.
func (p *AccountPool) IsCacheWarmed(accountID, cacheKey string) bool {
	if accountID == "" || cacheKey == "" {
		return false
	}
	p.mu.RLock()
	_, ok := p.cacheWarmed[cacheWarmedKey(accountID, cacheKey)]
	p.mu.RUnlock()
	return ok
}

// MarkCacheWarmed records that accountID has been warmed for cacheKey.
// Called after a successful warmup request, or after a real request that
// wrote to cache (cache_write_tokens > 0 or cached_tokens > 0 on a hit).
func (p *AccountPool) MarkCacheWarmed(accountID, cacheKey string) {
	if accountID == "" || cacheKey == "" {
		return
	}
	p.mu.Lock()
	p.cacheWarmed[cacheWarmedKey(accountID, cacheKey)] = true
	p.cacheWarmedTS[cacheWarmedKey(accountID, cacheKey)] = time.Now()
	p.mu.Unlock()
}

// IsCacheWarming reports whether a warmup request is currently in flight
// for the given accountID + cacheKey. Used to prevent duplicate warmups
// from concurrent requests.
func (p *AccountPool) IsCacheWarming(accountID, cacheKey string) bool {
	if accountID == "" || cacheKey == "" {
		return false
	}
	p.mu.RLock()
	_, ok := p.cacheWarming[cacheWarmedKey(accountID, cacheKey)]
	p.mu.RUnlock()
	return ok
}

// MarkCacheWarming records that a warmup request is in flight for the
// given accountID + cacheKey. Called BEFORE starting the async warmup
// goroutine to prevent concurrent requests from starting duplicates.
func (p *AccountPool) MarkCacheWarming(accountID, cacheKey string) {
	if accountID == "" || cacheKey == "" {
		return
	}
	p.mu.Lock()
	p.cacheWarming[cacheWarmedKey(accountID, cacheKey)] = true
	p.mu.Unlock()
}

// ClearCacheWarming removes the warming flag after the warmup completes
// (success or failure). On success, MarkCacheWarmed should be called
// first so the warmed flag is set before clearing the warming flag.
func (p *AccountPool) ClearCacheWarming(accountID, cacheKey string) {
	if accountID == "" || cacheKey == "" {
		return
	}
	p.mu.Lock()
	delete(p.cacheWarming, cacheWarmedKey(accountID, cacheKey))
	p.mu.Unlock()
}

// PruneCacheWarmed removes warmed entries older than the TTL. Called
// periodically from the background refresh loop. After expiry, the next
// request to that account+cacheKey will trigger a fresh warmup.
func (p *AccountPool) PruneCacheWarmed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cacheWarmedTTL <= 0 {
		return
	}
	cutoff := time.Now().Add(-p.cacheWarmedTTL)
	for key, ts := range p.cacheWarmedTS {
		if ts.Before(cutoff) {
			delete(p.cacheWarmed, key)
			delete(p.cacheWarmedTS, key)
		}
	}
}

// isModelLocked reports whether a specific model is in cooldown for this account.
func (p *AccountPool) isModelLocked(accountID, model string, now time.Time) bool {
	if model == "" {
		return false
	}
	locks, ok := p.modelLocks[accountID]
	if !ok || locks == nil {
		return false
	}
	until, ok := locks[model]
	return ok && now.Before(until)
}

// CountAccountsForModel returns how many accounts in the pool support the given model.
func (p *AccountPool) CountAccountsForModel(model string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, acc := range p.accounts {
		if p.accountHasModel(acc.ID, model) {
			count++
		}
	}
	return count
}

// GetByID returns an account by ID
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// RecordSuccess records a successful request and clears cooldown (and model lock)
func (p *AccountPool) RecordSuccess(id string, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	// Clear model lock for the specific model that succeeded
	if model != "" && p.modelLocks[id] != nil {
		delete(p.modelLocks[id], model)
		if len(p.modelLocks[id]) == 0 {
			delete(p.modelLocks, id)
		}
	}
}

// RecordError records a request error and sets model-level cooldown.
// Uses per-model locking so a
// quota/auth failure on one model doesn't block other models on the same account.
// model can be "" to fall back to account-level cooldown (legacy behaviour).
func (p *AccountPool) RecordError(id string, isQuotaError bool, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var cooldown time.Duration
	if isQuotaError {
		cooldown = time.Hour
	} else {
		p.errorCounts[id]++
		if p.errorCounts[id] >= 3 {
			cooldown = time.Minute
		}
	}

	if cooldown > 0 && model != "" {
		// Per-model lock: only this model on this account is cooled down
		if p.modelLocks[id] == nil {
			p.modelLocks[id] = make(map[string]time.Time)
		}
		p.modelLocks[id][model] = time.Now().Add(cooldown)
	} else if cooldown > 0 {
		// Legacy account-level cooldown
		p.cooldowns[id] = time.Now().Add(cooldown)
	}
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "HTTP 401 from ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// IsQuotaExhaustionError reports whether the error indicates the account's
// quota/credit/usage limit is HARD-exhausted (not a transient rate-limit blip).
// Such accounts will not recover on a same-account retry — the caller should
// rotate to a different account immediately instead of backing off.
//
// Recognised markers (case-insensitive):
//   - "credit limit exceeded" / "credit limit" (External OpenAI-compatible)
//   - "usage_limit_reached" / "usage limit" (Codex / ChatGPT subscription)
//   - "quota" + ("exceeded" / "exhausted" / "reached")
//   - "exceeded your current quota" (OpenAI standard)
//   - "insufficient_quota" / "insufficient quota"
//   - "plan_type" + "resets_at" (Codex usage_limit_reached body)
func IsQuotaExhaustionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())

	// Direct markers of hard quota/credit exhaustion.
	if strings.Contains(lower, "credit limit") ||
		strings.Contains(lower, "usage_limit") ||
		strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "insufficient quota") ||
		strings.Contains(lower, "exceeded your current quota") {
		return true
	}
	// "quota" paired with an exhaustion verb.
	if strings.Contains(lower, "quota") &&
		(strings.Contains(lower, "exceeded") ||
			strings.Contains(lower, "exhausted") ||
			strings.Contains(lower, "reached")) {
		return true
	}
	// Codex usage_limit_reached body carries plan_type + resets_at.
	if strings.Contains(lower, "plan_type") && strings.Contains(lower, "resets_at") {
		return true
	}
	return false
}

// IsTransientError reports whether the error is a transient upstream condition
// (provider overload, 5xx, timeout) that may succeed on a same-account retry
// after a short backoff. Unlike auth failures, the account stays enabled.
//
// Recognised markers:
//   - HTTP 502/503/504 (bad gateway / service unavailable / gateway timeout)
//   - "system cpu overloaded" / "system_*_overloaded" (bddevlab-style)
//   - "overloaded" / "rate_limit" / "rate limit" / "too many requests"
//   - "timeout" / "deadline exceeded" / "context deadline exceeded"
//   - "connection reset" / "EOF" / "broken pipe"
//   - "temporarily unavailable" / "service unavailable"
//
// NOTE: hard quota/credit exhaustion (IsQuotaExhaustionError) is NOT transient
// — retrying the same account wastes time. Callers should check
// IsQuotaExhaustionError first and rotate immediately.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Hard quota/credit exhaustion is NOT transient — don't retry same account.
	if IsQuotaExhaustionError(err) {
		return false
	}

	// HTTP 5xx status tokens (502/503/504) — bounded by non-digit boundaries
	// so we don't match arbitrary digits in error bodies.
	if hasStatusToken(msg, "502") || hasStatusToken(msg, "503") || hasStatusToken(msg, "504") {
		return true
	}

	// Upstream overload / rate-limit markers (covers bddevlab's
	// "system cpu overloaded", "system_cpu_overloaded", OpenAI's "overloaded",
	// and generic "rate limit" / "too many requests" patterns).
	if strings.Contains(lower, "overloaded") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "temporarily unavailable") ||
		strings.Contains(lower, "service unavailable") {
		return true
	}

	// Network / timeout markers.
	if strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "context deadline exceeded") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "eof") ||
		strings.Contains(lower, "no such host") {
		return true
	}

	return false
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// ClearCooldown removes the account-level cooldown and all per-model locks
// for the given account so the pool can pick it immediately. Used by the
// reset-quota admin endpoint to make an account fully available without
// waiting for the natural cooldown expiry.
func (p *AccountPool) ClearCooldown(id string) {
	p.mu.Lock()
	delete(p.cooldowns, id)
	delete(p.modelLocks, id)
	p.mu.Unlock()
}

// UpdateToken updates the account token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// Count returns total number of accounts
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount returns number of available accounts
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats updates account statistics. Counters live in p.stats keyed by
// accountID (not on the accounts[] routing slice), so Reload() rebuilding
// accounts[] no longer clobbers them. The updated totals are flushed to config
// asynchronously; the in-memory map remains the source of truth while running.
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s, ok := p.stats[id]
	if !ok {
		s = &accountStats{}
		p.stats[id] = s
	}
	s.RequestCount++
	s.TotalTokens += tokens
	s.TotalCredits += credits
	s.LastUsed = time.Now().Unix()

	go config.UpdateAccountStats(id, s.RequestCount, s.ErrorCount, s.TotalTokens, s.TotalCredits, s.LastUsed)
}

// ResetStats zeroes the in-memory per-account counters. Callers that also want
// the reset persisted must clear the config side separately
// (config.ResetAllAccountStats); this only clears the running source of truth so
// GetAllAccounts stops overlaying the old totals.
func (p *AccountPool) ResetStats() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats = make(map[string]*accountStats)
}

// GetAllAccounts returns a copy of all accounts with runtime stats overlaid
// from p.stats. The counters are read from the map (the running source of
// truth), not from the accounts[] copy, so callers see live totals even though
// Reload() rebuilds accounts[] from the (staler) persisted config.
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	for i := range result {
		if s, ok := p.stats[result[i].ID]; ok {
			result[i].RequestCount = s.RequestCount
			result[i].ErrorCount = s.ErrorCount
			result[i].TotalTokens = s.TotalTokens
			result[i].TotalCredits = s.TotalCredits
			result[i].LastUsed = s.LastUsed
		}
	}
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	if acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit {
		return true
	}
	// Codex accounts: treat primary usage percent >= 100 as exhausted so the
	// pool rotates to accounts that still have quota. Without this, accounts
	// with codexPrimaryUsedPercent=100 but UsageLimit=0 (Codex does not use the
	// Kiro UsageLimit fields) would never be skipped, causing upstream 429s.
	if acc.CodexPrimaryUsedPercent >= 100 {
		return true
	}
	return false
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	if isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage {
		return true
	}
	// External OpenAI-compatible: skip when credit balance is known-exhausted.
	// extStatus="exhausted" is the authoritative signal from the provider; the
	// numeric check (extCreditsUsed >= extCreditLimit) catches cases where the
	// status hasn't been refreshed yet but the numbers show an overdraft.
	if acc.ExtCreditLimit > 0 && acc.ExtCreditsUsed >= acc.ExtCreditLimit {
		return true
	}
	if strings.EqualFold(acc.ExtStatus, "exhausted") {
		return true
	}
	return false
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}
