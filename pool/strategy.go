// Package pool — routing strategy selection for large account pools (20+).
//
// The default round-robin strategy works well for small pools where every
// account has similar quota. For large pools (20+ accounts) with uneven
// usage — e.g. some accounts near their weekly reset, others freshly
// replenished — round-robin wastes requests on accounts that will return
// 429 mid-stream. Two opt-in strategies improve on this:
//
//   - "cost-optimized": prefer accounts with the most remaining quota
//     (lowest CodexPrimaryUsedPercent / highest ExtCreditsRemaining).
//     Minimises mid-stream 429s and lets low-usage accounts absorb burst
//     traffic.
//
//   - "reset-aware": avoid accounts whose quota window resets soon (under
//     resetAwareLeadTime, default 30m). An account that resets in 5 minutes
//     will likely 429 mid-stream; better to route to an account with a
//     longer window so the request completes, then let the resetting
//     account recover.
//
// Both strategies are pure tie-breakers among the accounts that already
// passed model/cooldown/quota filters in GetNextForModelExcluding. They do
// NOT replace the cacheSticky pinning — cacheSticky still wins because a
// cache hit saves far more quota than any strategy choice.
//
// Strategy is selected via the KVSettings key "poolRoutingStrategy":
//   "" / "round-robin" → default weighted round-robin (unchanged behaviour)
//   "cost-optimized"   → cost-optimized tie-break
//   "reset-aware"      → reset-aware tie-break
//
// The strategy only kicks in when the pool has >= strategyMinPoolSize
// accounts (default 20). Below that, round-robin is good enough and the
// strategy overhead (sorting candidates) is not worth paying.

package pool

import (
	"omniproxy/config"
	"sort"
	"time"
)

// strategyMinPoolSize is the pool size above which the cost-optimized and
// reset-aware strategies activate. Below this, round-robin is used regardless
// of the configured strategy — sorting a tiny pool adds overhead without
// meaningful benefit.
const strategyMinPoolSize = 20

// resetAwareLeadTime is how soon a quota window must reset before the
// reset-aware strategy starts avoiding the account. Accounts that reset
// within this window are deprioritised because a mid-stream 429 is likely.
const resetAwareLeadTime = 30 * time.Minute

// poolRoutingStrategy reads the configured strategy from KVSettings.
// Returns "round-robin" when unset or invalid.
func poolRoutingStrategy() string {
	s := config.GetStringSetting("poolRoutingStrategy", "round-robin")
	switch s {
	case "cost-optimized", "reset-aware", "round-robin":
		return s
	default:
		return "round-robin"
	}
}

// strategyShouldApply reports whether the active strategy should actually
// run for the current pool. Round-robin never needs to apply (it is the
// built-in behaviour); the other two only kick in for large pools.
func (p *AccountPool) strategyShouldApply() bool {
	strategy := poolRoutingStrategy()
	if strategy == "round-robin" {
		return false
	}
	// Count unique account IDs in the weighted slice.
	seen := make(map[string]bool, len(p.accounts))
	for i := range p.accounts {
		seen[p.accounts[i].ID] = true
	}
	return len(seen) >= strategyMinPoolSize
}

// scoreAccount returns a sort key for the given account under the active
// strategy. Lower score = preferred (sorted first). Caller already holds
// the pool RLock or is operating on a snapshot.
//
//   - round-robin: not used (caller falls back to atomic cursor rotation)
//   - cost-optimized: lower CodexPrimaryUsedPercent preferred; for external
//     accounts, higher ExtCreditsRemaining preferred (negated so more
//     remaining → lower score). Ties broken by lower TotalTokens (less
//     recent load).
//   - reset-aware: accounts whose CodexPrimaryResetAt is within
//     resetAwareLeadTime get a large penalty (+1000); otherwise score by
//     CodexPrimaryUsedPercent so among safe accounts we still prefer the
//     ones with more headroom.
func scoreAccount(acc config.Account, now time.Time) int {
	strategy := poolRoutingStrategy()
	switch strategy {
	case "cost-optimized":
		return scoreCostOptimized(acc)
	case "reset-aware":
		return scoreResetAware(acc, now)
	default:
		return 0
	}
}

// scoreCostOptimized returns a score where lower = more preferred.
func scoreCostOptimized(acc config.Account) int {
	// External OpenAI-compatible: rank by remaining credits (more = better).
	// Negate by subtracting from a large base so higher remaining → lower score.
	if acc.AuthMethod == "external_openai" && acc.ExtCreditLimit > 0 {
		remaining := acc.ExtCreditsRemaining
		if remaining < 0 {
			remaining = 0
		}
		// 1_000_000 - remaining: account with 5000 remaining → 995000,
		// account with 1000 remaining → 999000. Lower wins.
		return int(1_000_000 - remaining)
	}

	// Codex / Kiro: lower CodexPrimaryUsedPercent wins. When the field is
	// unset (0), treat as 0% used (most preferred) — this matches the
	// "freshly replenished" intent and avoids penalising accounts that
	// simply haven't been polled yet.
	pct := acc.CodexPrimaryUsedPercent
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	// Tie-break by cumulative tokens so equally-fresh accounts spread load
	// to the less-recently-used one. TotalTokens is int; cap contribution
	// so it never overrides the percent signal.
	tiebreak := acc.TotalTokens
	if tiebreak > 1000 {
		tiebreak = 1000
	}
	return pct*10 + tiebreak/100
}

// scoreResetAware returns a score where lower = more preferred.
// Accounts resetting within resetAwareLeadTime get a +1000 penalty.
func scoreResetAware(acc config.Account, now time.Time) int {
	base := scoreCostOptimized(acc) // among safe accounts, still prefer headroom

	// Only Codex accounts expose a reset timestamp. External/Kiro accounts
	// without a reset signal are treated as "safe" (no penalty).
	if acc.CodexPrimaryResetAt > 0 {
		resetAt := time.Unix(acc.CodexPrimaryResetAt, 0)
		if resetAt.Sub(now) < resetAwareLeadTime {
			// Large penalty so a resetting account is only picked when every
			// other account is excluded. The +1000 is bigger than the maximum
			// cost-optimized score (100*10 + 10 = 1010) so it cleanly
			// partitions the candidate list.
			return base + 10000
		}
	}
	return base
}

// pickByStrategy sorts the candidate accounts by the active strategy score
// and returns the first one. Candidates are accounts that already passed
// the model/cooldown/quota filters. Returns nil if candidates is empty.
//
// This is the strategy replacement for the atomic-cursor round-robin pick.
// It is only called when strategyShouldApply() returned true.
func pickByStrategy(candidates []config.Account, now time.Time) *config.Account {
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return scoreAccount(candidates[i], now) < scoreAccount(candidates[j], now)
	})
	// Return a pointer into the local slice — the caller treats it as
	// read-only and copies fields out via *acc dereference.
	return &candidates[0]
}
