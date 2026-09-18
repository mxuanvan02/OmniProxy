package pool

import (
	"omniproxy/config"
	"regexp"
	"sort"
	"strings"
	"time"
)

// aliasLocaleSuffixes are deployment-region suffixes that name the SAME model
// served from a different endpoint. They never change behaviour, so a request
// for the bare name may land on any of them when the bare name is unavailable.
// Behaviour-changing suffixes (-agent, -thinking-agent, -flash, effort levels)
// are deliberately absent: those are distinct models the client must ask for.
var aliasLocaleSuffixes = []string{"-cn", "-on"}

// aliasSnapshotSuffix matches dated snapshot suffixes such as "-0813". A
// snapshot is the same model frozen at a date; newer snapshots win.
var aliasSnapshotSuffix = regexp.MustCompile(`-\d{4}$`)

// canonicalModelKey reduces a model ID to its family key by dropping
// deployment-only suffixes (locale, dated snapshot) on top of the standard
// normalizeCatalogModelID work (case, provider prefix, [1m], claude dots).
// Two IDs with the same key are deploy variants of one model.
func canonicalModelKey(model string) string {
	key := normalizeCatalogModelID(model)
	for {
		prev := key
		for _, suffix := range aliasLocaleSuffixes {
			key = strings.TrimSuffix(key, suffix)
		}
		key = aliasSnapshotSuffix.ReplaceAllString(key, "")
		if key == prev {
			return key
		}
	}
}

// aliasRank orders same-family candidates. Lower rank is tried first;
// snapshot digits break ties inside the snapshot group (newer first) and the
// locale index inside the locale group, keeping the order deterministic.
type aliasRank struct {
	name   string
	rank   int
	snap   int
	locIdx int
}

func localeSuffixIndex(suffix string) int {
	for i, candidate := range aliasLocaleSuffixes {
		if suffix == candidate {
			return i
		}
	}
	return -1
}

func snapshotDigits(suffix string) int {
	if !aliasSnapshotSuffix.MatchString(suffix) {
		return 0
	}
	digits := 0
	for _, ch := range suffix[1:] {
		digits = digits*10 + int(ch-'0')
	}
	return digits
}

// rankAliasCandidates sorts same-family catalog names for a requested ID:
// exact match, then the bare family name, then locale variants in configured
// order, then dated snapshots newest first, then anything combined.
func rankAliasCandidates(requested, key string, names []string) []string {
	ranked := make([]aliasRank, 0, len(names))
	for _, name := range names {
		suffix := strings.TrimPrefix(name, key)
		entry := aliasRank{name: name, rank: 8, locIdx: len(aliasLocaleSuffixes), snap: snapshotDigits(suffix)}
		switch {
		case name == requested:
			entry.rank = 0
		case suffix == "":
			entry.rank = 1
		default:
			if idx := localeSuffixIndex(suffix); idx >= 0 {
				entry.rank, entry.locIdx = 2+idx, idx
			} else if entry.snap > 0 {
				entry.rank = 5
			}
		}
		ranked = append(ranked, entry)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.snap != b.snap {
			return a.snap > b.snap
		}
		if a.locIdx != b.locIdx {
			return a.locIdx < b.locIdx
		}
		return a.name < b.name
	})
	out := make([]string, len(ranked))
	for i, entry := range ranked {
		out[i] = entry.name
	}
	return out
}

// FindAvailableAliasModel returns the best deploy variant of the requested
// model that an eligible account can serve right now, or "" when the family
// has no other variant or none is healthy. It is a rescue path: callers invoke
// it only after the exact model came back unavailable, so happy-path routing
// never pays for the scan. Caller must not hold p.mu.
func (p *AccountPool) FindAvailableAliasModel(requested string) string {
	if p == nil || strings.TrimSpace(requested) == "" {
		return ""
	}
	wanted := normalizeCatalogModelID(requested)
	key := canonicalModelKey(requested)
	// Read config before taking p.mu to preserve the lock order used by Reload.
	allowOverUsage := config.GetAllowOverUsage()
	p.mu.RLock()
	defer p.mu.RUnlock()

	seen := make(map[string]bool)
	var names []string
	for _, list := range p.modelLists {
		for catalogName := range list {
			name := normalizeCatalogModelID(catalogName)
			if seen[name] || name == wanted {
				continue
			}
			if canonicalModelKey(name) != key {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}

	now := time.Now()
	for _, candidate := range rankAliasCandidates(wanted, key, names) {
		if p.aliasCandidateAvailable(candidate, now, allowOverUsage) {
			return candidate
		}
	}
	return ""
}

// aliasCandidateAvailable reports whether some distinct account can serve the
// candidate right now, mirroring the filters GetNextForModelExcluding applies
// so a variant is never offered into an account that would reject it.
func (p *AccountPool) aliasCandidateAvailable(candidate string, now time.Time, allowOverUsage bool) bool {
	seen := make(map[string]bool, len(p.accounts))
	for i := range p.accounts {
		acc := &p.accounts[i]
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if !p.accountHasModel(acc.ID, candidate) || p.isModelLocked(acc.ID, candidate, now) {
			continue
		}
		if until, ok := p.cooldowns[acc.ID]; ok && now.Before(until) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		return true
	}
	return false
}

// FindAvailableFallbackModel returns the first operator-configured fallback the
// pool can actually serve right now, or "" when none of them is healthy. Unlike
// FindAvailableAliasModel it crosses model families: it is the last rescue for a
// request the pool never served at all (a Claude name advertised on a Qwen-only
// pool), where no deploy variant exists to fall back to. fallbacks is the
// ordered Config.ModelFallbacks list; entries are normalized so the returned ID
// matches the catalog name the upstream gateway serves. Caller must not hold
// p.mu, and must only call this after the exact model and its same-family alias
// both came back unavailable, so the happy path never pays for the scan.
func (p *AccountPool) FindAvailableFallbackModel(fallbacks []string) string {
	if p == nil || len(fallbacks) == 0 {
		return ""
	}
	// Read config before taking p.mu to preserve the lock order used by Reload.
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[string]bool, len(fallbacks))
	for _, fallback := range fallbacks {
		candidate := normalizeCatalogModelID(fallback)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		if p.aliasCandidateAvailable(candidate, now, allowOverUsage) {
			return candidate
		}
	}
	return ""
}
