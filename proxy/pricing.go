package proxy

import (
	"strings"
	"sync"
)

// ModelPricing holds per-token pricing for a single model in USD per 1M tokens.
//
// Pricing is sourced from official provider pricing pages:
//   - OpenAI:    https://developers.openai.com/api/docs/pricing
//   - Anthropic: https://platform.claude.com/docs/en/about-claude/pricing
//
// CachedInput is the cache-read price (Anthropic "Cache Hits & Refreshes",
// OpenAI "cached input"). CacheCreate is the cache-write price — currently
// not tracked separately in usage records, so it is folded into InputPerM
// for cost computation (we treat all non-cached input as base input).
type ModelPricing struct {
	InputPerM  float64 `json:"inputPerM"`  // base input $/1M tokens
	CachedPerM float64 `json:"cachedPerM"` // cached input $/1M tokens (cache read)
	OutputPerM float64 `json:"outputPerM"` // output $/1M tokens
	// PerCallUSD is a flat charge per request. Reseller gateways running
	// one-api's "quota_type 1" bill per call and ignore token counts entirely,
	// so when this is set it REPLACES the per-token rates instead of adding to
	// them.
	PerCallUSD float64 `json:"perCallUsd,omitempty"`
	Source     string  `json:"source"` // "openai" | "anthropic" | "provider" | "custom"
	Notes      string  `json:"notes,omitempty"`
}

// pricingTable is the canonical pricing map keyed by exact model name.
// Models not in this map fall back via resolveModelPricing (prefix match).
var pricingTable = map[string]ModelPricing{
	// ─── OpenAI GPT-5.6 family (https://developers.openai.com/api/docs/pricing) ───
	"gpt-5.6-sol":   {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 30.0, Source: "openai"},
	"gpt-5.6-terra": {InputPerM: 2.0, CachedPerM: 0.2, OutputPerM: 12.0, Source: "openai"},
	"gpt-5.6-luna":  {InputPerM: 0.2, CachedPerM: 0.02, OutputPerM: 1.2, Source: "openai"},
	// GPT-5.1 family
	"gpt-5.1": {InputPerM: 1.25, CachedPerM: 0.125, OutputPerM: 10.0, Source: "openai"},

	// ─── Anthropic Claude family (https://platform.claude.com/docs/en/about-claude/pricing) ───
	// Fable 5 and Mythos 5 share one list price; the naming difference is about
	// safeguards, not cost. The .1 refresh keeps $10/$50 and cuts cache reads to
	// 2.5% of input instead of the 10% every other Claude model charges.
	"claude-fable-5":    {InputPerM: 10.0, CachedPerM: 1.0, OutputPerM: 50.0, Source: "anthropic"},
	"claude-fable-5.1":  {InputPerM: 10.0, CachedPerM: 0.25, OutputPerM: 50.0, Source: "anthropic", Notes: "Cache reads cut from $1.00 to $0.25 in the 5.1 refresh"},
	"claude-mythos-5":   {InputPerM: 10.0, CachedPerM: 1.0, OutputPerM: 50.0, Source: "anthropic"},
	"claude-mythos-5.1": {InputPerM: 10.0, CachedPerM: 0.25, OutputPerM: 50.0, Source: "anthropic", Notes: "Mirrors Fable 5.1: same list price, cache reads cut to $0.25"},
	// Opus 5 — same $5/$25 as Opus 4.8
	"claude-opus-5": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	// Opus 4.x — flat $5/$0.50 cache read/$25 output since Opus 4.5
	"claude-opus-4.8": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	"claude-opus-4.7": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	"claude-opus-4.6": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	"claude-opus-4.5": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	// Sonnet 5 — $2/$0.20 cache/$10 output. Launched as an introductory rate
	// through 2026-08-31, but the $3/$15 step-up was cancelled on 2026-08-10.
	"claude-sonnet-5":   {InputPerM: 2.0, CachedPerM: 0.2, OutputPerM: 10.0, Source: "anthropic", Notes: "Launch rate made permanent on 2026-08-10; the planned $3/$0.30/$15 increase was cancelled"},
	"claude-sonnet-4.6": {InputPerM: 3.0, CachedPerM: 0.3, OutputPerM: 15.0, Source: "anthropic"},
	"claude-sonnet-4.5": {InputPerM: 3.0, CachedPerM: 0.3, OutputPerM: 15.0, Source: "anthropic"},
	// Haiku 4.5
	"claude-haiku-4.5": {InputPerM: 1.0, CachedPerM: 0.1, OutputPerM: 5.0, Source: "anthropic"},

	// ─── SOTA family (https://www.sotamodel.net/api/pricing) ───
	// The gateway publishes ratios, not dollars: input = model_ratio * $2,
	// output = input * completion_ratio, cache read = input * cache_ratio.
	// Verified against claude-opus-5 on the same endpoint (ratio 2.5,
	// completion 5, cache 0.1 → $5/$0.50/$25), which matches Anthropic's
	// published rate, so the $2 base is sound.
	"model-s": {InputPerM: 10.0, CachedPerM: 1.0, OutputPerM: 50.0, Source: "sota", Notes: "Derived from gateway ratios: model_ratio 5, completion_ratio 5, cache_ratio 0.1"},
	"model-t": {InputPerM: 3.0, CachedPerM: 0.3, OutputPerM: 15.0, Source: "sota", Notes: "Derived from gateway ratios: model_ratio 1.5, completion_ratio 5, cache_ratio 0.1"},
	"model-o": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "sota", Notes: "Derived from gateway ratios: model_ratio 2.5, completion_ratio 5, cache_ratio 0.1"},
	"model-a": {InputPerM: 5.5, CachedPerM: 0.55, OutputPerM: 27.5, Source: "sota", Notes: "Derived from gateway ratios: model_ratio 2.75, completion_ratio 5, cache_ratio 0.1"},
}

// customPricing allows operators to override or add pricing via config without
// recompiling. Keyed by exact model name. Takes precedence over pricingTable.
var customPricing sync.Map // map[string]ModelPricing

// SetCustomPricing installs an operator-provided pricing override for a model.
// Pass a zero-value ModelPricing to clear the override and fall back to the
// built-in table.
func SetCustomPricing(model string, p ModelPricing) {
	if model == "" {
		return
	}
	// A flat per-call override carries no per-token rates, so PerCallUSD has to
	// count as a real price here or storing one would delete it instead.
	if p.InputPerM == 0 && p.OutputPerM == 0 && p.CachedPerM == 0 && p.PerCallUSD == 0 {
		customPricing.Delete(model)
		return
	}
	customPricing.Store(model, p)
}

// LookupPricing returns the pricing for a model, checking custom overrides
// first, then the built-in table, then a prefix-based fallback. The bool
// indicates whether a real pricing entry was found (false = unknown model,
// returned pricing is zero-value).
func LookupPricing(model string) (ModelPricing, bool) {
	if model == "" {
		return ModelPricing{}, false
	}
	// Strip common provider prefixes ("codex/", "kr/", "codezdev/", etc.).
	// Lowercase because upstream advertises the SOTA family as "model-S" while
	// the table is keyed lowercase; a case mismatch silently bills $0.
	clean := strings.ToLower(stripModelPrefix(model))
	// 1. Custom override (exact match)
	if v, ok := customPricing.Load(clean); ok {
		return v.(ModelPricing), true
	}
	// 2. Built-in table (exact match)
	if p, ok := pricingTable[clean]; ok {
		return p, true
	}
	// 3. Prefix fallback — match "claude-opus-4" → first claude-opus-4.* entry
	if p, ok := prefixFallbackPricing(clean); ok {
		return p, true
	}
	return ModelPricing{}, false
}

// stripModelPrefix removes provider routing prefixes so "codex/gpt-5.6-sol"
// resolves to "gpt-5.6-sol" and "kr/claude-sonnet-5" to "claude-sonnet-5".
func stripModelPrefix(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// prefixFallbackPricing matches a model name against known family prefixes
// when an exact match fails. Useful for new minor versions ("claude-opus-4.9")
// that share pricing with their family.
func prefixFallbackPricing(model string) (ModelPricing, bool) {
	switch {
	// The .1 refreshes must be tested before their bare family prefix, which
	// would otherwise swallow them and bill the old $1.00 cache-read rate.
	case strings.HasPrefix(model, "claude-fable-5.1"):
		return pricingTable["claude-fable-5.1"], true
	case strings.HasPrefix(model, "claude-mythos-5.1"):
		return pricingTable["claude-mythos-5.1"], true
	case strings.HasPrefix(model, "claude-fable-5"):
		return pricingTable["claude-fable-5"], true
	case strings.HasPrefix(model, "claude-mythos-5"):
		return pricingTable["claude-mythos-5"], true
	case strings.HasPrefix(model, "claude-opus-5"):
		return pricingTable["claude-opus-5"], true
	case strings.HasPrefix(model, "claude-opus-4"):
		return pricingTable["claude-opus-4.8"], true
	case strings.HasPrefix(model, "claude-sonnet-5"):
		return pricingTable["claude-sonnet-5"], true
	case strings.HasPrefix(model, "claude-sonnet-4"):
		return pricingTable["claude-sonnet-4.6"], true
	case strings.HasPrefix(model, "claude-haiku-4"):
		return pricingTable["claude-haiku-4.5"], true
	case strings.HasPrefix(model, "gpt-5.6-sol"):
		return pricingTable["gpt-5.6-sol"], true
	case strings.HasPrefix(model, "gpt-5.6-terra"):
		return pricingTable["gpt-5.6-terra"], true
	case strings.HasPrefix(model, "gpt-5.6-luna"):
		return pricingTable["gpt-5.6-luna"], true
	case strings.HasPrefix(model, "gpt-5.1"):
		return pricingTable["gpt-5.1"], true
	}
	return ModelPricing{}, false
}

// CostBreakdown holds the per-component USD cost of a request, computed from
// the model pricing. CachedCost is billed at the cache-read rate, InputCost at
// the base input rate (for the uncached portion), OutputCost at the output
// rate. Total = InputCost + CachedCost + OutputCost.
type CostBreakdown struct {
	InputCost  float64 `json:"inputCost"`  // (input - cached) * InputPerM / 1M
	CachedCost float64 `json:"cachedCost"` // cached * CachedPerM / 1M
	OutputCost float64 `json:"outputCost"` // output * OutputPerM / 1M
	Total      float64 `json:"total"`      // sum of the three
}

// ComputeCostBreakdown returns the per-component USD cost of a request against
// the model vendor's list price. If the model is unknown, all fields are zero.
// `cached` is the cache-hit token count (a subset of `input`); it is clamped to
// `input` if it exceeds it.
//
// Prefer ComputeCostBreakdownForAccount when the account is known: a reseller
// gateway's own price for the same model is usually not the vendor's.
func ComputeCostBreakdown(model string, input, cached, output int) CostBreakdown {
	return ComputeCostBreakdownForAccount("", model, input, cached, output)
}

// ComputeCostBreakdownForAccount prices a request against the gateway the
// account actually bills through, falling back to the vendor list price when
// that gateway publishes no price list.
func ComputeCostBreakdownForAccount(accountID, model string, input, cached, output int) CostBreakdown {
	p, ok := lookupAccountPricing(accountID, model)
	if !ok {
		if p, ok = LookupPricing(model); !ok {
			return CostBreakdown{}
		}
	}
	// A flat per-request charge ignores token counts, so it has no per-component
	// breakdown to report.
	if p.PerCallUSD > 0 {
		return CostBreakdown{Total: p.PerCallUSD}
	}
	if cached > input {
		cached = input
	}
	uncached := input - cached
	bd := CostBreakdown{
		InputCost:  float64(uncached) * p.InputPerM / 1_000_000.0,
		CachedCost: float64(cached) * p.CachedPerM / 1_000_000.0,
		OutputCost: float64(output) * p.OutputPerM / 1_000_000.0,
	}
	bd.Total = bd.InputCost + bd.CachedCost + bd.OutputCost
	return bd
}

// ComputeCost calculates the real USD cost of a request given token counts
// and the model's pricing. Cost = (input-cached)*InputPerM + cached*CachedPerM
// + output*OutputPerM, all divided by 1M. If the model is unknown, returns 0.
//
// `cached` is the cache-hit token count (a subset of `input`). Cache-create
// tokens are currently billed at the base input rate — we do not have a
// separate cache-write price field in the usage record, so they fold into
// `input` to avoid double-counting.
func ComputeCost(model string, input, cached, output int) float64 {
	return ComputeCostBreakdown(model, input, cached, output).Total
}

// ResolveCredits returns the credit figure to book against an account. Only
// Codex reports a real credit charge (x-codex-credits-* headers); every
// OpenAI-compatible provider reports none, which left the admin UI's per-account
// CREDITS column at 0.0 forever. Fall back to the price the account's own
// gateway charges — or the vendor list price when it publishes none — so the
// column reflects actual spend for those accounts.
func ResolveCredits(accountID string, upstreamCredits float64, model string, input, cached, output int) float64 {
	if upstreamCredits > 0 {
		return upstreamCredits
	}
	return ComputeCostBreakdownForAccount(accountID, model, input, cached, output).Total
}

// EffectiveTokens returns the "real" tokens consumed = (input - cached) + output.
// This is the metric to display when the user wants to know how many tokens
// were actually processed (not served from cache).
func EffectiveTokens(input, cached, output int) int {
	if cached > input {
		cached = input
	}
	return (input - cached) + output
}
