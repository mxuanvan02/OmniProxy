package proxy

import (
	"encoding/json"
	"net/http"
	"omniproxy/config"
	"sort"
	"strings"
	"time"
)

// ─── Quota Tracker API ──────────────────────────────────────────────────────

// quotaProviderSummary aggregates quota across all accounts of a provider type.
type quotaProviderSummary struct {
	Provider       string  `json:"provider"`
	Label          string  `json:"label"`
	Accounts       int     `json:"accounts"`
	ActiveAccounts int     `json:"activeAccounts"`
	UsageCurrent   float64 `json:"usageCurrent"`
	UsageLimit     float64 `json:"usageLimit"`
	UsagePercent   float64 `json:"usagePercent"`
	// Codex-specific
	CodexPrimaryUsedPercent   *int   `json:"codexPrimaryUsedPercent,omitempty"`
	CodexSecondaryUsedPercent *int   `json:"codexSecondaryUsedPercent,omitempty"`
	CodexPrimaryResetAt       *int64 `json:"codexPrimaryResetAt,omitempty"`
	CodexSecondaryResetAt     *int64 `json:"codexSecondaryResetAt,omitempty"`
	// External credits
	ExtCreditLimit      *float64 `json:"extCreditLimit,omitempty"`
	ExtCreditsRemaining *float64 `json:"extCreditsRemaining,omitempty"`
	ExtCreditsUsed      *float64 `json:"extCreditsUsed,omitempty"`
}

// quotaAccountRow is a per-account quota breakdown for the Quota Tracker page.
type quotaAccountRow struct {
	ID               string  `json:"id"`
	Email            string  `json:"email"`
	Nickname         string  `json:"nickname"`
	Provider         string  `json:"provider"`
	ProviderLabel    string  `json:"providerLabel"`
	Enabled          bool    `json:"enabled"`
	SubscriptionType string  `json:"subscriptionType"`
	UsageCurrent     float64 `json:"usageCurrent"`
	UsageLimit       float64 `json:"usageLimit"`
	UsagePercent     float64 `json:"usagePercent"`
	NextResetDate    string  `json:"nextResetDate"`
	DaysRemaining    int     `json:"daysRemaining"`
	AddedAt          int64   `json:"addedAt,omitempty"`
	// Quotas is the 9router-style per-account quota rows: each row has its own
	// name, used, total, remaining %, resetAt, and recurring flag. Multiple
	// rows per account are common (Codex primary+secondary, Qoder personal+org).
	Quotas []quotaRow `json:"quotas"`
	// Codex (kept for backward compat)
	CodexPlanType              string `json:"codexPlanType,omitempty"`
	CodexPrimaryUsedPercent    int    `json:"codexPrimaryUsedPercent,omitempty"`
	CodexSecondaryUsedPercent  int    `json:"codexSecondaryUsedPercent,omitempty"`
	CodexPrimaryResetAt        int64  `json:"codexPrimaryResetAt,omitempty"`
	CodexSecondaryResetAt      int64  `json:"codexSecondaryResetAt,omitempty"`
	CodexCreditsBalance        *int   `json:"codexCreditsBalance,omitempty"`
	CodexCreditsUnlimited      bool   `json:"codexCreditsUnlimited,omitempty"`
	CodexResetCreditsAvailable int    `json:"codexResetCreditsAvailable,omitempty"`
	// External
	ExtCreditLimit      float64 `json:"extCreditLimit,omitempty"`
	ExtCreditsRemaining float64 `json:"extCreditsRemaining,omitempty"`
	ExtCreditsUsed      float64 `json:"extCreditsUsed,omitempty"`
	ExtStatus           string  `json:"extStatus,omitempty"`
	// Trial
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"`
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`
	TrialStatus       string  `json:"trialStatus,omitempty"`
	// Status: derived human-readable status badge (e.g. "exhausted", "active", "unlimited", "banned")
	Status string `json:"status,omitempty"`
	// BanStatus: raw ban status from account ("ACTIVE", "BANNED", "SUSPENDED", "DISABLED")
	BanStatus string `json:"banStatus,omitempty"`
}

// quotaRow is a single quota bar inside an account block (9router-style).
type quotaRow struct {
	Name      string  `json:"name"`              // display name: "Primary", "Secondary", "Usage", "Credits"
	Used      float64 `json:"used"`              // current usage
	Total     float64 `json:"total"`             // limit (0 = unlimited)
	Remaining int     `json:"remaining"`         // remaining % (0-100)
	ResetAt   *int64  `json:"resetAt,omitempty"` // unix seconds
	Recurring bool    `json:"recurring"`         // true = resets periodically, false = one-time credits
	Unit      string  `json:"unit,omitempty"`    // "credits", "tokens", "%", etc.
}

// apiGetQuotaOverview GET /admin/api/quota/overview
// Returns aggregate quota by provider + per-account breakdown.
//
// Uses h.pool.GetAllAccountsFull() which returns ALL config accounts
// (including disabled, banned, and quota-blocked) with live pool stats
// overlaid. This ensures:
//   - All accounts visible on the Quota page (not just routable ones)
//   - SUM of per-account tokens/requests == /status total tokens/requests
//   - Banned/disabled accounts show their historical usage
func (h *Handler) apiGetQuotaOverview(w http.ResponseWriter, r *http.Request) {
	accounts := h.pool.GetAllAccountsFull()

	providerSummaries := map[string]*quotaProviderSummary{
		"kiro":     {Provider: "kiro", Label: "Kiro / CodeWhisperer"},
		"codex":    {Provider: "codex", Label: "Codex (ChatGPT)"},
		"external": {Provider: "external", Label: "External OpenAI-compatible"},
		"trial":    {Provider: "trial", Label: "Trial"},
	}

	var accountRows []quotaAccountRow
	for _, a := range accounts {
		providerLabel := providerLabelOf(a.Provider)
		row := quotaAccountRow{
			ID:               a.ID,
			Email:            a.Email,
			Nickname:         a.Nickname,
			Provider:         a.Provider,
			ProviderLabel:    providerLabel,
			Enabled:          a.Enabled,
			SubscriptionType: a.SubscriptionType,
			UsageCurrent:     a.UsageCurrent,
			UsageLimit:       a.UsageLimit,
			UsagePercent:     a.UsagePercent,
			NextResetDate:    a.NextResetDate,
			DaysRemaining:    a.DaysRemaining,
			AddedAt:          a.AddedAt,
		}

		// The upstream reports a percentage and the primary reset deadline, but
		// not a token count. Derive that count from the persisted per-account
		// usage aggregation for the current upstream-reported window.
		windowTokens := 0
		if start, ok := codexPrimaryWindowStart(a); ok && h.usageTracker != nil {
			windowTokens = h.usageTracker.TokensForAccountSince(a.ID, start)
		}

		// Build 9router-style quotas[] array — one row per quota dimension.
		row.Quotas = buildAccountQuotas(a, windowTokens)

		// Codex fields (backward compat)
		if a.CodexPlanType != "" || a.CodexPrimaryUsedPercent > 0 || isCodexAccount(&a) {
			row.CodexPlanType = a.CodexPlanType
			row.CodexPrimaryUsedPercent = a.CodexPrimaryUsedPercent
			row.CodexSecondaryUsedPercent = a.CodexSecondaryUsedPercent
			row.CodexPrimaryResetAt = a.CodexPrimaryResetAt
			row.CodexSecondaryResetAt = a.CodexSecondaryResetAt
			row.CodexCreditsBalance = &a.CodexCreditsBalance
			row.CodexCreditsUnlimited = a.CodexCreditsUnlimited
			row.CodexResetCreditsAvailable = a.CodexResetCreditsAvailable
		}

		// External credits
		if a.ExtCreditLimit > 0 || a.ExtCreditsRemaining > 0 {
			row.ExtCreditLimit = a.ExtCreditLimit
			row.ExtCreditsRemaining = a.ExtCreditsRemaining
			row.ExtCreditsUsed = a.ExtCreditsUsed
			row.ExtStatus = a.ExtStatus
		}

		// Trial
		if a.TrialUsageLimit > 0 {
			row.TrialUsageCurrent = a.TrialUsageCurrent
			row.TrialUsageLimit = a.TrialUsageLimit
			row.TrialStatus = a.TrialStatus
		}

		// Derived status badge + raw ban status
		row.BanStatus = a.BanStatus
		row.Status = deriveAccountStatus(a)

		accountRows = append(accountRows, row)

		// Aggregate into provider summaries
		ps := pickProviderSummary(a, providerSummaries)
		if ps == nil {
			continue
		}
		ps.Accounts++
		if a.Enabled {
			ps.ActiveAccounts++
		}
		ps.UsageCurrent += a.UsageCurrent
		ps.UsageLimit += a.UsageLimit
	}

	// Compute aggregate percentages
	for _, ps := range providerSummaries {
		if ps.UsageLimit > 0 {
			ps.UsagePercent = (ps.UsageCurrent / ps.UsageLimit) * 100
		}
	}

	// Sort accounts: disabled last, then by earliest quota reset (expiring first)
	sort.SliceStable(accountRows, func(i, j int) bool {
		if accountRows[i].Enabled != accountRows[j].Enabled {
			return accountRows[i].Enabled
		}
		return earliestResetAt(accountRows[i].Quotas) < earliestResetAt(accountRows[j].Quotas)
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers": providerSummaries,
		"accounts":  accountRows,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// providerLabelOf maps an internal provider string to a human-readable label.
func providerLabelOf(p string) string {
	switch p {
	case "BuilderId":
		return "Kiro / CodeWhisperer"
	case "ExternalIdp":
		return "External IdP"
	case "OpenAI Codex", "codex":
		return "Codex (ChatGPT)"
	case "external":
		return "External OpenAI-compatible"
	case "trial":
		return "Trial"
	default:
		if p == "" {
			return "Kiro / CodeWhisperer"
		}
		return p
	}
}

// earliestResetAt returns the earliest non-zero resetAt unix seconds in a
// quota row list, or math.MaxInt64 if none have reset times (sorts last).
func earliestResetAt(quotas []quotaRow) int64 {
	var earliest int64 = 1<<63 - 1
	for _, q := range quotas {
		if q.ResetAt != nil && *q.ResetAt > 0 && *q.ResetAt < earliest {
			earliest = *q.ResetAt
		}
	}
	return earliest
}

// remainingPct returns the remaining percentage given used/total.
// If total is 0 or absent, returns 100 (unlimited). If used >= total, returns 0.
func remainingPct(used, total float64) int {
	if total == 0 {
		return 100
	}
	if used >= total {
		return 0
	}
	return int((total - used) / total * 100)
}

func codexPrimaryQuotaName(windowMinutes int) string {
	if windowMinutes == 5*60 {
		return "Primary (5h)"
	}
	return "Primary"
}

// codexPrimaryWindowStart returns the start of the quota window currently
// reported by Codex. Both values must come from upstream; without either one
// there is no defensible boundary for a "This Reset" token total.
func codexPrimaryWindowStart(a config.Account) (time.Time, bool) {
	if a.CodexPrimaryResetAt <= 0 || a.CodexPrimaryWindowMinutes <= 0 {
		return time.Time{}, false
	}
	resetAt := time.Unix(a.CodexPrimaryResetAt, 0).UTC()
	window := time.Duration(a.CodexPrimaryWindowMinutes) * time.Minute
	if window <= 0 {
		return time.Time{}, false
	}
	return resetAt.Add(-window), true
}

// buildAccountQuotas constructs the 9router-style quotas[] array for one
// account, emitting one row per quota dimension (Codex primary/secondary,
// Kiro usage, External credits, Trial). Empty/unlimited quotas are omitted.
func buildAccountQuotas(a config.Account, tokensThisReset int) []quotaRow {
	var rows []quotaRow

	// Codex primary quota. The upstream window duration decides whether a 5h
	// label is accurate; the current 7-day window remains simply "Primary".
	if a.CodexPrimaryUsedPercent > 0 || a.CodexPrimaryResetAt > 0 {
		usedPct := a.CodexPrimaryUsedPercent
		remaining := 100 - usedPct
		if remaining < 0 {
			remaining = 0
		}
		r := quotaRow{
			Name:      codexPrimaryQuotaName(a.CodexPrimaryWindowMinutes),
			Used:      float64(usedPct),
			Total:     100,
			Remaining: remaining,
			Recurring: true,
			Unit:      "%",
		}
		if a.CodexPrimaryResetAt > 0 {
			ts := a.CodexPrimaryResetAt
			r.ResetAt = &ts
		}
		rows = append(rows, r)
	}

	// Codex secondary (weekly window)
	if a.CodexSecondaryUsedPercent > 0 || a.CodexSecondaryResetAt > 0 {
		usedPct := a.CodexSecondaryUsedPercent
		remaining := 100 - usedPct
		if remaining < 0 {
			remaining = 0
		}
		r := quotaRow{
			Name:      "Secondary (weekly)",
			Used:      float64(usedPct),
			Total:     100,
			Remaining: remaining,
			Recurring: true,
			Unit:      "%",
		}
		if a.CodexSecondaryResetAt > 0 {
			ts := a.CodexSecondaryResetAt
			r.ResetAt = &ts
		}
		rows = append(rows, r)
	}

	// Codex purchased credits (one-time, non-recurring)
	if !a.CodexCreditsUnlimited && a.CodexCreditsBalance > 0 {
		rows = append(rows, quotaRow{
			Name:      "Credits",
			Used:      float64(a.CodexCreditsBalance),
			Total:     0, // balance-only, no known limit
			Remaining: 100,
			Recurring: false,
			Unit:      "credits",
		})
	}
	if a.CodexCreditsUnlimited {
		rows = append(rows, quotaRow{
			Name:      "Credits",
			Used:      0,
			Total:     0,
			Remaining: 100,
			Recurring: false,
			Unit:      "∞",
		})
	}

	// Codex token counters: cumulative lifetime total plus the current primary
	// quota-window total derived from persisted usage. Do not read the legacy
	// runtime counter here: its lifecycle is independent from the durable usage
	// records and it can legitimately be stale after a restart or reset.
	isCodex := a.CodexPlanType != "" || a.CodexPrimaryUsedPercent > 0 || a.ChatGPTAccountID != "" ||
		strings.Contains(strings.ToLower(a.Provider), "codex") || a.AuthMethod == "codex"
	if isCodex {
		rows = append(rows, quotaRow{
			Name:      "Total Tokens",
			Used:      float64(a.TotalTokens),
			Total:     0,
			Remaining: 100,
			Recurring: false,
			Unit:      "tokens",
		})
		if _, ok := codexPrimaryWindowStart(a); ok {
			rows = append(rows, quotaRow{
				Name:      "Tokens This Reset",
				Used:      float64(tokensThisReset),
				Total:     0,
				Remaining: 100,
				Recurring: true,
				Unit:      "tokens",
			})
		}
		if a.RequestCount > 0 {
			rows = append(rows, quotaRow{
				Name:      "Requests",
				Used:      float64(a.RequestCount),
				Total:     0,
				Remaining: 100,
				Recurring: false,
				Unit:      "reqs",
			})
		}
	}

	// Kiro / CodeWhisperer usage
	if a.UsageLimit > 0 || a.UsageCurrent > 0 {
		r := quotaRow{
			Name:      "Usage",
			Used:      a.UsageCurrent,
			Total:     a.UsageLimit,
			Remaining: remainingPct(a.UsageCurrent, a.UsageLimit),
			Recurring: true,
		}
		// Parse NextResetDate → unix seconds if available
		if a.NextResetDate != "" {
			if t, err := time.Parse(time.RFC3339, a.NextResetDate); err == nil {
				ts := t.Unix()
				r.ResetAt = &ts
			}
		}
		rows = append(rows, r)
	}

	// External OpenAI-compatible: credits (if tracked) + tokens/requests (always)
	isExternal := a.AuthMethod == "external_openai" ||
		strings.Contains(strings.ToLower(a.Provider), "external")
	if isExternal {
		// Credits row — only if a credit limit is tracked
		if a.ExtCreditLimit > 0 || a.ExtCreditsUsed > 0 || a.ExtStatus != "" {
			used := a.ExtCreditsUsed
			total := a.ExtCreditLimit
			// Cap remaining at 0 when overdraft (used > total)
			remaining := 100
			if total > 0 {
				if used >= total {
					remaining = 0
				} else {
					remaining = int((total - used) / total * 100)
				}
			}
			r := quotaRow{
				Name:      "Credits",
				Used:      used,
				Total:     total,
				Remaining: remaining,
				Recurring: false,
				Unit:      "credits",
			}
			rows = append(rows, r)
		}
		// Tokens row — always show for external (cumulative, unlimited)
		// Use a.TotalTokens (same source as /status and /accounts) for
		// consistency. ExtTokensUsed is a legacy field that may diverge.
		if a.TotalTokens > 0 {
			rows = append(rows, quotaRow{
				Name:      "Tokens",
				Used:      float64(a.TotalTokens),
				Total:     0, // unlimited / cumulative
				Remaining: 100,
				Recurring: false,
				Unit:      "tokens",
			})
		}
		// Requests row — always show for external
		// Use a.RequestCount (same source as /status and /accounts).
		if a.RequestCount > 0 {
			rows = append(rows, quotaRow{
				Name:      "Requests",
				Used:      float64(a.RequestCount),
				Total:     0,
				Remaining: 100,
				Recurring: false,
				Unit:      "reqs",
			})
		}
	} else {
		// Non-external: legacy External credits handling (rare)
		if a.ExtCreditLimit > 0 || a.ExtCreditsRemaining > 0 {
			used := a.ExtCreditsUsed
			if used == 0 && a.ExtCreditLimit > 0 {
				used = a.ExtCreditLimit - a.ExtCreditsRemaining
			}
			rows = append(rows, quotaRow{
				Name:      "Credits",
				Used:      used,
				Total:     a.ExtCreditLimit,
				Remaining: remainingPct(used, a.ExtCreditLimit),
				Recurring: false,
				Unit:      "credits",
			})
		}
	}

	// Trial
	if a.TrialUsageLimit > 0 {
		rows = append(rows, quotaRow{
			Name:      "Trial",
			Used:      a.TrialUsageCurrent,
			Total:     a.TrialUsageLimit,
			Remaining: remainingPct(a.TrialUsageCurrent, a.TrialUsageLimit),
			Recurring: false,
		})
	}

	return rows
}

// deriveAccountStatus returns a short status label for badge display.
// Priority: banned > disabled > exhausted > unlimited > active.
func deriveAccountStatus(a config.Account) string {
	// Banned takes top priority — account is blocked by provider
	bs := strings.ToUpper(strings.TrimSpace(a.BanStatus))
	if bs == "BANNED" {
		return "banned"
	}
	if bs == "SUSPENDED" {
		return "suspended"
	}
	if bs == "DISABLED" {
		return "disabled"
	}
	if !a.Enabled {
		return "disabled"
	}
	// External: use extStatus if present
	if a.AuthMethod == "external_openai" || strings.Contains(strings.ToLower(a.Provider), "external") {
		if a.ExtStatus != "" {
			return a.ExtStatus // "exhausted", "active", etc.
		}
		if a.ExtCreditLimit > 0 && a.ExtCreditsUsed >= a.ExtCreditLimit {
			return "exhausted"
		}
		return "active"
	}
	// Codex: check primary/secondary usage
	if a.CodexCreditsUnlimited {
		return "unlimited"
	}
	if a.CodexPrimaryUsedPercent >= 100 {
		return "exhausted"
	}
	if a.CodexPrimaryUsedPercent > 0 {
		return "active"
	}
	// Kiro / default
	if a.UsageLimit > 0 && a.UsageCurrent >= a.UsageLimit {
		return "exhausted"
	}
	return "active"
}

func pickProviderSummary(a config.Account, m map[string]*quotaProviderSummary) *quotaProviderSummary {
	if a.CodexPlanType != "" || a.CodexPrimaryUsedPercent > 0 || a.ChatGPTAccountID != "" {
		return m["codex"]
	}
	if a.ExtCreditLimit > 0 || a.ExtCreditsRemaining > 0 || a.BaseURL != "" {
		return m["external"]
	}
	if a.TrialUsageLimit > 0 {
		return m["trial"]
	}
	return m["kiro"]
}

// ─── Cache Stats API ────────────────────────────────────────────────────────

// apiGetCacheStats GET /admin/api/cache/stats
// Returns aggregated cache hit/miss stats from the usage tracker.
func (h *Handler) apiGetCacheStats(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "24h"
	}

	stats := h.usageTracker.GetStats(period)

	// Build cache-specific summary
	cacheRead := stats.TotalCacheReadTokens
	cacheCreate := stats.TotalCacheCreateTokens
	cachedTokens := stats.TotalCachedTokens
	totalInput := stats.TotalPromptTokens
	cacheHits := cacheHitTokens(
		totalInput,
		stats.TotalCompletionTokens,
		stats.TotalEffectiveTokens,
		cacheRead,
		cachedTokens,
	)

	// Cache-create tokens are billed input, not cache hits. Keep them separate
	// from the saved-token calculation while making all parts add up to input.
	uncached := totalInput - cacheHits - cacheCreate
	if uncached < 0 {
		uncached = 0
	}

	cacheTotal := cacheHits + cacheCreate + uncached
	hitRatio := 0.0
	if cacheTotal > 0 {
		hitRatio = float64(cacheHits) / float64(cacheTotal) * 100
	}

	// Per-account cache breakdown
	byAccount := make(map[string]map[string]int)
	for id, s := range stats.ByAccount {
		byAccount[id] = map[string]int{
			"cacheRead":        s.CacheReadTokens,
			"cacheCreate":      s.CacheCreateTokens,
			"cachedTokens":     s.CachedTokens,
			"promptTokens":     s.PromptTokens,
			"completionTokens": s.CompletionTokens,
			"effectiveTokens":  s.EffectiveTokens,
			"cacheHits": cacheHitTokens(
				s.PromptTokens,
				s.CompletionTokens,
				s.EffectiveTokens,
				s.CacheReadTokens,
				s.CachedTokens,
			),
			"requests": s.Requests,
		}
	}

	// Per-model cache breakdown
	byModel := make(map[string]map[string]int)
	for model, s := range stats.ByModel {
		byModel[model] = map[string]int{
			"cacheRead":        s.CacheReadTokens,
			"cacheCreate":      s.CacheCreateTokens,
			"cachedTokens":     s.CachedTokens,
			"promptTokens":     s.PromptTokens,
			"completionTokens": s.CompletionTokens,
			"effectiveTokens":  s.EffectiveTokens,
			"cacheHits": cacheHitTokens(
				s.PromptTokens,
				s.CompletionTokens,
				s.EffectiveTokens,
				s.CacheReadTokens,
				s.CachedTokens,
			),
			"requests": s.Requests,
		}
	}

	// Tokens saved are only cache hits. Cache-create tokens remain billable input.
	tokensSaved := cacheHits
	// Estimated cost savings (rough: cached tokens cost ~10% of normal)
	estimatedSavingsPct := 0.0
	if totalInput > 0 {
		estimatedSavingsPct = float64(tokensSaved) / float64(totalInput) * 100
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"period":       period,
		"totalInput":   totalInput,
		"cacheRead":    cacheRead,
		"cacheCreate":  cacheCreate,
		"cachedTokens": cachedTokens,
		"cacheHits":    cacheHits,
		"uncached":     uncached,
		"tokensSaved":  tokensSaved,
		"hitRatio":     hitRatio,
		"savingsPct":   estimatedSavingsPct,
		"byAccount":    byAccount,
		"byModel":      byModel,
		"accountNames": stats.AccountNames,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	})
}
