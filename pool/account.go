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

// Routing affinity must outlive the upstream cache window rather than expire
// inside it: while the upstream entry is still warm, rotating to a cold account
// pays a full cache create for a prefix that already exists elsewhere. The
// window is therefore per-provider, because the documented cache lifetimes
// differ by an order of magnitude and a single constant is wrong for one of
// them no matter which value is chosen.
//
// defaultCacheStickyTTL covers Anthropic-shaped upstreams: the ephemeral cache
// runs a 5-minute TTL refreshed on every hit, so one minute of headroom keeps a
// live entry recoverable while still releasing affinity once it is truly gone.
const defaultCacheStickyTTL = 6 * time.Minute

// openAIModernCacheStickyTTL covers GPT-5.6 and later, where the documented
// prompt-cache lifetime is 30 minutes (the only supported value) and is
// refreshed on reuse at no extra cost. Holding affinity for only 6 minutes threw
// away the remaining 24 minutes of a paid-for cache entry.
const openAIModernCacheStickyTTL = 31 * time.Minute

// openAILegacyCacheStickyTTL covers pre-5.6 OpenAI models, where in-memory
// prefixes are documented to survive roughly 5-10 minutes of inactivity rather
// than a fixed 30. Extended retention exists but is opt-in per request, so the
// conservative end of the documented range is the safe affinity window: holding
// longer would skew load balancing with no cache left to recover.
const openAILegacyCacheStickyTTL = 11 * time.Minute

// stickyTTLForModel resolves how long routing affinity should be held for a
// model, keyed on the upstream that actually owns the cache entry.
//
// Matching is on the model id rather than the account's auth method because the
// same OpenAI-family model can be served through several account types, and it
// is the model's upstream — not the credential — that determines the cache
// lifetime.
func stickyTTLForModel(model string, fallback time.Duration) time.Duration {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return fallback
	}
	isOpenAIFamily := strings.HasPrefix(lower, "gpt-") ||
		strings.Contains(lower, "codex") ||
		strings.HasPrefix(lower, "o1") ||
		strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "o4")
	if !isOpenAIFamily {
		return fallback
	}
	// Both spellings occur because gateways rewrite model ids inconsistently.
	if strings.Contains(lower, "5.6") || strings.Contains(lower, "5-6") {
		return openAIModernCacheStickyTTL
	}
	return openAILegacyCacheStickyTTL
}

// accountStats holds the cumulative runtime counters for a single account.
// It lives in AccountPool.stats keyed by accountID, deliberately separate from
// the accounts[] routing slice: Reload() rebuilds accounts[] from config on
// every account mutation (40+ call sites), so counters stored on the slice were
// silently reset by racing reloads. Keeping them here means routing rebuilds no
// longer clobber usage totals.
type accountStats struct {
	RequestCount                 int
	ErrorCount                   int
	TotalTokens                  int
	CodexTokensSincePrimaryReset int
	CodexPrimaryResetAt          int64
	TotalCredits                 float64
	LastUsed                     int64
}

// AccountPool manages the account pool
type AccountPool struct {
	mu              sync.RWMutex
	accounts        []config.Account
	serviceAccounts []config.Account
	totalAccounts   int
	currentIndex    uint64
	serviceIndex    uint64
	cooldowns       map[string]time.Time            // account cooldown time
	errorCounts     map[string]int                  // consecutive error count
	modelLists      map[string]map[string]bool      // accountID → set of modelIDs (from ListAvailableModels)
	modelLocks      map[string]map[string]time.Time // accountID → modelName → cooldown until
	stats           map[string]*accountStats        // accountID → cumulative runtime stats (survives Reload)
	// cacheSticky maps a model-scoped prompt-cache key
	// to the account ID that last handled it. Used to pin consecutive turns
	// from the same conversation to the same upstream account so the
	// provider's prompt cache can warm up and serve hits.
	cacheSticky    map[string]string    // "model\x00cacheKey" → accountID
	cacheStickyTS  map[string]time.Time // "model\x00cacheKey" → last seen time
	cacheStickyTTL time.Duration
	// cacheWarmed tracks (accountID + model + cacheKey) tuples that have already been
	// warmed via a background warmup request. Used by on-rotation warming to
	// avoid re-warming the same account for the same cache key (which would
	// waste tokens and rate-limit budget).
	cacheWarmed    map[string]bool      // "accountID\x00model\x00cacheKey" → true
	cacheWarmedTS  map[string]time.Time // "accountID\x00model\x00cacheKey" → warm time
	cacheWarmedTTL time.Duration        // expire warmed entries before upstream cache does
	// cacheWarming tracks (accountID + model + cacheKey) tuples that currently have
	// a warmup request in flight. Prevents duplicate warmups from concurrent
	// requests to the same account+cacheKey.
	cacheWarming map[string]bool // "accountID\x00model\x00cacheKey" → true
	// Warmup failures are scoped to the actual upstream endpoint. A DNS or
	// service outage therefore pauses background warmups across all accounts
	// sharing that endpoint, without affecting foreground inference requests.
	cacheWarmFailureCount map[string]int
	cacheWarmRetryAfter   map[string]time.Time
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool returns the global account pool singleton
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:             make(map[string]time.Time),
			errorCounts:           make(map[string]int),
			modelLists:            make(map[string]map[string]bool),
			modelLocks:            make(map[string]map[string]time.Time),
			stats:                 make(map[string]*accountStats),
			cacheSticky:           make(map[string]string),
			cacheStickyTS:         make(map[string]time.Time),
			cacheStickyTTL:        defaultCacheStickyTTL,
			cacheWarmed:           make(map[string]bool),
			cacheWarmedTS:         make(map[string]time.Time),
			cacheWarmedTTL:        4 * time.Minute, // stay inside the typical 5m upstream cache window
			cacheWarming:          make(map[string]bool),
			cacheWarmFailureCount: make(map[string]int),
			cacheWarmRetryAfter:   make(map[string]time.Time),
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
	if p.cooldowns == nil {
		p.cooldowns = make(map[string]time.Time)
	}
	if p.errorCounts == nil {
		p.errorCounts = make(map[string]int)
	}
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.modelLocks == nil {
		p.modelLocks = make(map[string]map[string]time.Time)
	}
	if p.stats == nil {
		p.stats = make(map[string]*accountStats)
	}
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	var weighted []config.Account
	var services []config.Account
	for _, a := range enabled {
		if accountSupportsServiceCapability(a) {
			if !isQuotaBlocked(a, allowOverUsage) {
				services = append(services, a)
			}
		}
		if accountSupportsCapability(a, "chat") {
			w := effectiveWeight(a.Weight)
			if isQuotaBlocked(a, allowOverUsage) {
				continue
			}
			for j := 0; j < w; j++ {
				weighted = append(weighted, a)
			}
			continue
		}
	}
	p.accounts = weighted
	p.serviceAccounts = services
	p.totalAccounts = len(enabled)

	// Seed runtime stats from the persisted config counters, but only for
	// accounts we are not already tracking in memory. The in-memory map is the
	// source of truth once the process is running (it accumulates every request
	// and is flushed to config asynchronously); overwriting it here would undo
	// increments that raced ahead of the last flush — the original under-count
	// bug. New accounts (or a fresh process) legitimately start from config.
	for _, a := range enabled {
		if _, ok := p.stats[a.ID]; ok {
			continue
		}
		p.stats[a.ID] = &accountStats{
			RequestCount:                 a.RequestCount,
			ErrorCount:                   a.ErrorCount,
			TotalTokens:                  a.TotalTokens,
			CodexTokensSincePrimaryReset: a.CodexTokensSincePrimaryReset,
			CodexPrimaryResetAt:          a.CodexPrimaryResetAt,
			TotalCredits:                 a.TotalCredits,
			LastUsed:                     a.LastUsed,
		}
	}
}

// serviceCapabilities are the non-chat capabilities served from the service
// pool. They are listed explicitly rather than derived because the chat pool and
// the service pool are partitioned on this answer: a credential that lands in
// the wrong one is either unreachable or exposed to chat traffic it cannot
// serve. audio-tts and video belong here because media providers (Gommo) expose
// them without any chat model, so an account configured only for speech or
// video would otherwise join no pool at all and never be selected.
var serviceCapabilities = []string{"search", "image", "audio-tts", "audio-music", "video"}

func accountSupportsServiceCapability(account config.Account) bool {
	for _, capability := range serviceCapabilities {
		if accountSupportsCapability(account, capability) {
			return true
		}
	}
	return false
}

func accountSupportsCapability(account config.Account, capability string) bool {
	if capability == "" {
		return true
	}
	// ProviderKind was persisted by earlier service-account imports before
	// explicit Capabilities became mandatory. Treat it as a capability fallback
	// so those accounts remain routable after reload.
	if strings.EqualFold(strings.TrimSpace(account.ProviderKind), capability) {
		return true
	}
	if len(account.Capabilities) > 0 {
		for _, value := range account.Capabilities {
			if strings.EqualFold(strings.TrimSpace(value), capability) {
				return true
			}
		}
		return false
	}
	// Legacy accounts predate explicit capabilities and remain chat-routable.
	return capability == "chat"
}

// GetNextForCapability selects a service account without exposing search or
// image credentials to the normal model pool. provider is optional; when it is
// empty, all accounts with the requested capability participate in failover.
func (p *AccountPool) GetNextForCapability(capability, provider string, excluded map[string]bool) *config.Account {
	// Read config before taking p.mu: holding the pool lock across a cfgLock
	// acquisition serialises all capability routing against chat routing.
	allowOverUsage := config.GetAllowOverUsage()
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.serviceAccounts) == 0 {
		return nil
	}
	now := time.Now()
	for i := 0; i < len(p.serviceAccounts); i++ {
		idx := int(atomic.AddUint64(&p.serviceIndex, 1) % uint64(len(p.serviceAccounts)))
		account := &p.serviceAccounts[idx]
		if excluded != nil && excluded[account.ID] {
			continue
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(account.Provider), strings.TrimSpace(provider)) {
			continue
		}
		if !accountSupportsCapability(*account, capability) || isQuotaBlocked(*account, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[account.ID]; ok && now.Before(cooldown) {
			continue
		}
		if locks := p.modelLocks[account.ID]; locks != nil {
			if until := locks[capability]; now.Before(until) {
				continue
			}
		}
		selected := *account
		return &selected
	}
	return nil
}

// GetNextCodex selects a Codex subscription account for capability-specific
// requests such as image generation. Codex accounts remain in the normal chat
// pool, so they must not be selected through serviceAccounts.
func (p *AccountPool) GetNextCodex(excluded map[string]bool) *config.Account {
	// Read config before taking p.mu, for the same reason as
	// GetNextForCapability: selection is read-only and must not serialise
	// against chat routing through cfgLock.
	allowOverUsage := config.GetAllowOverUsage()
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.accounts) == 0 {
		return nil
	}
	now := time.Now()
	seen := make(map[string]bool)
	for i := 0; i < len(p.accounts); i++ {
		idx := int(atomic.AddUint64(&p.currentIndex, 1) % uint64(len(p.accounts)))
		account := &p.accounts[idx]
		if seen[account.ID] || (excluded != nil && excluded[account.ID]) {
			seen[account.ID] = true
			continue
		}
		seen[account.ID] = true
		if account.AuthMethod != "codex" || isQuotaBlocked(*account, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[account.ID]; ok && now.Before(cooldown) {
			continue
		}
		if locks := p.modelLocks[account.ID]; locks != nil {
			if until := locks["image"]; now.Before(until) {
				continue
			}
		}
		// Return a copy: callers mutate token fields on the account they get
		// back, and a pointer into p.accounts would race UpdateToken/other
		// selectors and be orphaned by Reload replacing the slice.
		selected := *account
		return &selected
	}
	return nil
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

		selected := *acc
		return &selected
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
			selected := *acc
			return &selected
		}
	}
	if best == nil {
		return nil
	}
	selected := *best
	return &selected
}

// SetModelList caches the model set for an account (called by handler after refresh).
// An empty response means the catalog is unavailable/unknown, not that the
// account supports no models. Preserve any previously known catalog and leave
// a previously uncached account optimistic so a transient /v1/models failure
// cannot filter it out before the inference request reaches the provider.
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	if len(modelIDs) == 0 {
		return
	}
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

// accountHasModel checks if the account supports the requested model. A missing
// catalog is treated optimistically during cold start, but once a catalog has
// been loaded it is authoritative for every account type, including external
// OpenAI-compatible providers.
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok {
		return true // cold start: catalog not loaded yet
	}
	if len(list) == 0 {
		return false
	}
	requested := normalizeCatalogModelID(model)
	for catalogModel := range list {
		candidate := normalizeCatalogModelID(catalogModel)
		if candidate == requested || strings.HasPrefix(candidate, requested+"-") {
			return true
		}
	}
	return false
}

// hasKnownModelCatalog reports whether model discovery produced a non-empty
// catalog for an account. An absent catalog is deliberately distinct from a
// known catalog that does not contain the requested model.
func (p *AccountPool) hasKnownModelCatalog(accountID string) bool {
	list, ok := p.modelLists[accountID]
	return ok && len(list) > 0
}

func normalizeCatalogModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		model = strings.TrimSpace(model[idx+1:])
	}
	model = strings.TrimSuffix(model, "[1m]")
	if strings.HasPrefix(model, "claude-") {
		model = strings.ReplaceAll(model, ".", "-")
	}
	return model
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
			return isExternalAuthMethod(p.accounts[i].AuthMethod)
		}
	}
	return false
}

func isExternalAuthMethod(authMethod string) bool {
	switch strings.ToLower(strings.TrimSpace(authMethod)) {
	case "external_openai", "agentrouter", "external_agentrouter":
		return true
	default:
		return false
	}
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
	var preferredKnown []config.Account
	var preferredUnknown []config.Account
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
				return isExternalAuthMethod(acc.AuthMethod)
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
				} else if preferExternal && !p.hasKnownModelCatalog(picked.ID) {
					// A missing catalog is an unknown capability state. Keep it as
					// cold-start fallback, but never let it outrank an external
					// account whose catalog explicitly contains the requested model.
					preferredUnknown = append(preferredUnknown, *picked)
				} else {
					preferredKnown = append(preferredKnown, *picked)
				}
				continue
			}
		}
		// Strategy mode: if we collected preferred candidates, pick the best
		// one by score and return it without falling through to Phase 2.
		if useStrategy && len(preferredCandidates) > 0 {
			return pickByStrategy(preferredCandidates, now)
		}
		if !useStrategy && len(preferredKnown) > 0 {
			return &preferredKnown[0]
		}
		if !useStrategy && len(preferredUnknown) > 0 {
			return &preferredUnknown[0]
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
		selected := *acc
		return &selected
	}

	// Strategy mode: pick the best-scoring candidate from Phase 2.
	if useStrategy && len(allCandidates) > 0 {
		return pickByStrategy(allCandidates, now)
	}

	return nil
}

// GetNextForModelWithCacheKey works like GetNextForModelExcluding but first
// tries the account that last handled the same model + cacheKey. This keeps
// consecutive turns from the same conversation on the same upstream account
// so the provider's prompt cache can warm up and serve hits.
//
// cacheKey is the opaque prefix hash derived from the instructions (see
// codexCacheKey). When cacheKey is empty, falls back to normal rotation.
func (p *AccountPool) GetNextForModelWithCacheKey(model string, excluded map[string]bool, cacheKey string) *config.Account {
	if cacheKey != "" {
		stickyKey := cacheStickyKey(model, cacheKey)
		p.mu.RLock()
		stickyID, ok := p.cacheSticky[stickyKey]
		stickyAt := p.cacheStickyTS[stickyKey]
		stickyTTL := stickyTTLForModel(model, p.cacheStickyTTL)
		p.mu.RUnlock()
		if ok && stickyID != "" && (stickyTTL <= 0 || time.Since(stickyAt) < stickyTTL) {
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
		selected := *acc
		return &selected
	}
	return nil
}

// PeekCacheSticky reports which account is currently pinned for model+cacheKey
// and whether that pin is still inside the TTL. Read-only: it neither creates
// nor refreshes a pin, so callers can use it purely for observability without
// perturbing the routing they are trying to measure.
func (p *AccountPool) PeekCacheSticky(model, cacheKey string) (string, bool) {
	if model == "" || cacheKey == "" {
		return "", false
	}
	key := cacheStickyKey(model, cacheKey)
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.cacheSticky[key]
	if !ok || id == "" {
		return "", false
	}
	// Must resolve the same per-model TTL the selection path uses, otherwise a
	// live 31-minute GPT-5.6 pin would be reported as expired against the
	// 6-minute default and the diagnostics would blame the wrong cause.
	ttl := stickyTTLForModel(model, p.cacheStickyTTL)
	if ttl > 0 && time.Since(p.cacheStickyTS[key]) >= ttl {
		return id, false
	}
	return id, true
}

// RecordCacheStickiness pins model + cacheKey → accountID so subsequent requests
// with the same model and prefix prefer this account. Called after a successful
// upstream response.
func (p *AccountPool) RecordCacheStickiness(model, cacheKey, accountID string) {
	if model == "" || cacheKey == "" || accountID == "" {
		return
	}
	key := cacheStickyKey(model, cacheKey)
	p.mu.Lock()
	if p.cacheSticky == nil {
		p.cacheSticky = make(map[string]string)
		p.cacheStickyTS = make(map[string]time.Time)
	}
	p.cacheSticky[key] = accountID
	p.cacheStickyTS[key] = time.Now()
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
	// Prune per-key, not against one global cutoff: the sticky key embeds the
	// model, and TTLs now differ five-fold between providers. A single
	// 6-minute cutoff would evict live 31-minute GPT-5.6 pins on the next
	// refresh tick and silently undo the longer affinity window.
	now := time.Now()
	for key, ts := range p.cacheStickyTS {
		model := key
		if idx := strings.IndexByte(key, 0); idx >= 0 {
			model = key[:idx]
		}
		ttl := stickyTTLForModel(model, p.cacheStickyTTL)
		if ttl <= 0 {
			continue
		}
		if now.Sub(ts) >= ttl {
			delete(p.cacheSticky, key)
			delete(p.cacheStickyTS, key)
		}
	}
}

func cacheStickyKey(model, cacheKey string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + cacheKey
}

func cacheWarmedKey(accountID, model, cacheKey string) string {
	return accountID + "\x00" + strings.ToLower(strings.TrimSpace(model)) + "\x00" + cacheKey
}

func cacheWarmScopeKey(upstreamScope string) string {
	return strings.ToLower(strings.TrimSpace(upstreamScope))
}

// TryStartCacheWarm atomically reserves a model-specific prefix warmup. It
// prevents duplicate in-flight work, honours the upstream cache lifetime, and
// pauses background work while that upstream endpoint is backing off.
func (p *AccountPool) TryStartCacheWarm(accountID, model, cacheKey, upstreamScope string) bool {
	if accountID == "" || model == "" || cacheKey == "" {
		return false
	}
	key := cacheWarmedKey(accountID, model, cacheKey)
	scope := cacheWarmScopeKey(upstreamScope)
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cacheWarmed == nil {
		p.cacheWarmed = make(map[string]bool)
		p.cacheWarmedTS = make(map[string]time.Time)
		p.cacheWarming = make(map[string]bool)
		p.cacheWarmFailureCount = make(map[string]int)
		p.cacheWarmRetryAfter = make(map[string]time.Time)
	}
	if warmedAt, ok := p.cacheWarmedTS[key]; ok {
		if p.cacheWarmedTTL <= 0 || now.Sub(warmedAt) < p.cacheWarmedTTL {
			return false
		}
		delete(p.cacheWarmed, key)
		delete(p.cacheWarmedTS, key)
	}
	if p.cacheWarming[key] {
		return false
	}
	if retryAfter, ok := p.cacheWarmRetryAfter[scope]; ok && now.Before(retryAfter) {
		return false
	}
	p.cacheWarming[key] = true
	return true
}

// CompleteCacheWarm marks the model-specific prefix as warm and clears any
// background-warmup circuit breaker for the upstream endpoint.
func (p *AccountPool) CompleteCacheWarm(accountID, model, cacheKey, upstreamScope string) {
	if accountID == "" || model == "" || cacheKey == "" {
		return
	}
	key := cacheWarmedKey(accountID, model, cacheKey)
	scope := cacheWarmScopeKey(upstreamScope)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cacheWarmed == nil {
		p.cacheWarmed = make(map[string]bool)
		p.cacheWarmedTS = make(map[string]time.Time)
		p.cacheWarming = make(map[string]bool)
		p.cacheWarmFailureCount = make(map[string]int)
		p.cacheWarmRetryAfter = make(map[string]time.Time)
	}
	p.cacheWarmed[key] = true
	p.cacheWarmedTS[key] = time.Now()
	delete(p.cacheWarming, key)
	delete(p.cacheWarmFailureCount, scope)
	delete(p.cacheWarmRetryAfter, scope)
}

// FailCacheWarm releases the in-flight reservation and applies an exponential
// backoff to all background warmups sharing the failed upstream endpoint.
func (p *AccountPool) FailCacheWarm(accountID, model, cacheKey, upstreamScope string) {
	if accountID == "" || model == "" || cacheKey == "" {
		return
	}
	key := cacheWarmedKey(accountID, model, cacheKey)
	scope := cacheWarmScopeKey(upstreamScope)
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cacheWarming, key)
	if scope == "" {
		return
	}
	if p.cacheWarmFailureCount == nil {
		p.cacheWarmFailureCount = make(map[string]int)
		p.cacheWarmRetryAfter = make(map[string]time.Time)
	}
	count := p.cacheWarmFailureCount[scope] + 1
	p.cacheWarmFailureCount[scope] = count
	delay := 30 * time.Second
	for i := 1; i < count && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	p.cacheWarmRetryAfter[scope] = time.Now().Add(delay)
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
	now := time.Now()
	for scope, retryAfter := range p.cacheWarmRetryAfter {
		if !now.Before(retryAfter) {
			delete(p.cacheWarmRetryAfter, scope)
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

// GetByID returns an account by ID.
// Service-only accounts (image/video/tts providers with no "chat" capability)
// never enter p.accounts, so the serviceAccounts list must be searched too —
// otherwise a media account is invisible to every caller that resolves an ID.
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			selected := p.accounts[i]
			return &selected
		}
	}
	for i := range p.serviceAccounts {
		if p.serviceAccounts[i].ID == id {
			selected := p.serviceAccounts[i]
			return &selected
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
	// Some OpenAI-compatible gateways report throttling as HTTP 403. The
	// structured rate-limit marker is authoritative: refreshing credentials
	// cannot fix it and only delays account rotation.
	if IsRateLimitError(err) {
		return false
	}

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

// IsRateLimitError reports provider throttling regardless of the HTTP status
// chosen by the gateway. In particular, some gateways use HTTP 403 with an
// OpenAI-style rate_limit_error body instead of HTTP 429.
func IsRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	return hasStatusToken(msg, "429") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests")
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

// IsProviderModelUnavailableError reports a provider-local model capability
// failure. The provider may be healthy, but it cannot serve the requested model;
// retrying the same account is wasteful and the caller should rotate to another
// eligible account/provider immediately.
func IsProviderModelUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	modelMarker := strings.Contains(lower, "model")
	if !modelMarker {
		return false
	}
	return (strings.Contains(lower, "not available") &&
		(strings.Contains(lower, "provider") || strings.Contains(lower, "configured"))) ||
		strings.Contains(lower, "model unavailable") ||
		strings.Contains(lower, "unavailable on this provider") ||
		(strings.Contains(lower, "no available provider") && strings.Contains(lower, "model"))
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
//   - HTTP/2 stream resets with INTERNAL_ERROR / REFUSED_STREAM
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

	// A provider-local model capability failure is not transient for this
	// account. Retrying the same provider wastes the retry budget; callers must
	// rotate to another eligible account/provider instead.
	if IsProviderModelUnavailableError(err) {
		return false
	}

	// A stream that never starts is already bounded by the transport/read
	// deadline. Repeating it on the same account only multiplies latency; rotate
	// immediately so the request can use its limited account failover budget.
	if strings.Contains(lower, "stream idle timeout") ||
		strings.Contains(lower, "timeout awaiting response headers") {
		return false
	}

	// Hard quota/credit exhaustion is NOT transient — don't retry same account.
	if IsQuotaExhaustionError(err) || IsRateLimitError(err) {
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
		strings.Contains(lower, "no such host") ||
		isTransientHTTP2StreamReset(lower) {
		return true
	}

	return false
}

func isTransientHTTP2StreamReset(lower string) bool {
	if !strings.Contains(lower, "stream error:") {
		return false
	}
	return strings.Contains(lower, "internal_error") ||
		strings.Contains(lower, "refused_stream")
}

// IsContentBlockedError reports whether err indicates the upstream refused
// the request payload or model (e.g. AgentRouter HTTP 400 "content-blocked").
// This is a payload-level refusal, not an account fault. Callers must NOT
// rotate accounts — the same payload will fail identically on every account.
func IsContentBlockedError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "content-blocked") ||
		strings.Contains(lower, "content_blocker") ||
		strings.Contains(lower, "content blocked")
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
	for i := range p.serviceAccounts {
		if p.serviceAccounts[i].ID == id {
			p.serviceAccounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.serviceAccounts[i].RefreshToken = refreshToken
			}
			p.serviceAccounts[i].ExpiresAt = expiresAt
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
// accounts[] no longer clobbers them. The updated totals are persisted before
// this method returns so a request cannot leave a fire-and-forget config write
// behind after its lifecycle ends.
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()

	s, ok := p.stats[id]
	if !ok {
		// An account can receive traffic before Reload has seeded p.stats
		// (for example, after a runtime account mutation). Start from the
		// persisted snapshot so the write carries its current reset ID.
		s = &accountStats{}
		for _, account := range config.GetAccounts() {
			if account.ID != id {
				continue
			}
			s = &accountStats{
				RequestCount:                 account.RequestCount,
				ErrorCount:                   account.ErrorCount,
				TotalTokens:                  account.TotalTokens,
				CodexTokensSincePrimaryReset: account.CodexTokensSincePrimaryReset,
				CodexPrimaryResetAt:          account.CodexPrimaryResetAt,
				TotalCredits:                 account.TotalCredits,
				LastUsed:                     account.LastUsed,
			}
			break
		}
		p.stats[id] = s
	}
	s.RequestCount++
	s.TotalTokens += tokens
	if p.isCodexAccountLocked(id) {
		s.CodexTokensSincePrimaryReset += tokens
	}
	s.TotalCredits += credits
	s.LastUsed = time.Now().Unix()

	requestCount := s.RequestCount
	errorCount := s.ErrorCount
	totalTokens := s.TotalTokens
	codexTokensSincePrimaryReset := s.CodexTokensSincePrimaryReset
	codexPrimaryResetAt := s.CodexPrimaryResetAt
	totalCredits := s.TotalCredits
	lastUsed := s.LastUsed
	p.mu.Unlock()

	// Persist outside p.mu. UpdateAccountStats takes config's lock and may
	// perform an atomic file write; holding the pool lock across that operation
	// would unnecessarily block routing and risk lock-order inversions.
	_ = config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, codexTokensSincePrimaryReset, codexPrimaryResetAt, totalCredits, lastUsed)
}

func (p *AccountPool) isCodexAccountLocked(id string) bool {
	for _, account := range p.accounts {
		if account.ID == id {
			return account.AuthMethod == "codex"
		}
	}
	for _, account := range config.GetAccounts() {
		if account.ID == id {
			return account.AuthMethod == "codex"
		}
	}
	return false
}

// ResetCodexPrimaryWindowTokens clears the live per-window token counter
// after the upstream reports a new Codex primary reset timestamp.
func (p *AccountPool) ResetCodexPrimaryWindowTokens(id string, resetAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if stats, ok := p.stats[id]; ok {
		stats.CodexTokensSincePrimaryReset = 0
		stats.CodexPrimaryResetAt = resetAt
	}
}

// SyncCodexPrimaryWindow aligns live stats with the canonical deadline stored
// after the first usage header of a window. When an account was added during
// that window before this counter existed, bootstrap its window total from the
// cumulative runtime total.
func (p *AccountPool) SyncCodexPrimaryWindow(id string, resetAt int64, bootstrapTokens bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if stats, ok := p.stats[id]; ok {
		stats.CodexPrimaryResetAt = resetAt
		if bootstrapTokens && stats.TotalTokens > stats.CodexTokensSincePrimaryReset {
			stats.CodexTokensSincePrimaryReset = stats.TotalTokens
		}
	}
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
			result[i].CodexTokensSincePrimaryReset = s.CodexTokensSincePrimaryReset
			result[i].CodexPrimaryResetAt = s.CodexPrimaryResetAt
			result[i].TotalCredits = s.TotalCredits
			result[i].LastUsed = s.LastUsed
		}
	}
	return result
}

// GetAllAccountsFull returns ALL config accounts (including disabled,
// banned, and quota-blocked ones) with live pool stats overlaid on top.
// Unlike GetAllAccounts() which only returns routable pool accounts,
// this returns every account the operator has configured — so the Quota
// page and /status can show a complete picture with consistent totals.
//
// Accounts not currently in the pool (disabled/banned) retain their
// last-persisted stats from config; accounts in the pool get live
// in-memory stats overlaid. This ensures:
//   - SUM of per-account tokens/requests == /status total tokens/requests
//   - All accounts visible on Quota page (not just routable ones)
//   - Banned/disabled accounts show their historical usage
func (p *AccountPool) GetAllAccountsFull() []config.Account {
	all := config.GetAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range all {
		if s, ok := p.stats[all[i].ID]; ok {
			all[i].RequestCount = s.RequestCount
			all[i].ErrorCount = s.ErrorCount
			all[i].TotalTokens = s.TotalTokens
			all[i].CodexTokensSincePrimaryReset = s.CodexTokensSincePrimaryReset
			all[i].CodexPrimaryResetAt = s.CodexPrimaryResetAt
			all[i].TotalCredits = s.TotalCredits
			all[i].LastUsed = s.LastUsed
		}
	}
	return all
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
