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
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool returns the global account pool singleton
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:   make(map[string]time.Time),
			errorCounts: make(map[string]int),
			modelLists:  make(map[string]map[string]bool),
			modelLocks:  make(map[string]map[string]time.Time),
			stats:       make(map[string]*accountStats),
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
		return acc
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
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

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
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
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
