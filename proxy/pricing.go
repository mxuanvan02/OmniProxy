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
	Source     string  `json:"source"`     // "openai" | "anthropic" | "custom"
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
	// Opus 4.x — flat $5/$0.50 cache read/$25 output since Opus 4.5
	"claude-opus-4.8": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	"claude-opus-4.7": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	"claude-opus-4.6": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	"claude-opus-4.5": {InputPerM: 5.0, CachedPerM: 0.5, OutputPerM: 25.0, Source: "anthropic"},
	// Sonnet 5 — promotional $2/$0.20 cache/$10 output through Aug 31, 2026
	"claude-sonnet-5":   {InputPerM: 2.0, CachedPerM: 0.2, OutputPerM: 10.0, Source: "anthropic", Notes: "Promotional rate through 2026-08-31; $3/$0.30/$15 after"},
	"claude-sonnet-4.6": {InputPerM: 3.0, CachedPerM: 0.3, OutputPerM: 15.0, Source: "anthropic"},
	"claude-sonnet-4.5": {InputPerM: 3.0, CachedPerM: 0.3, OutputPerM: 15.0, Source: "anthropic"},
	// Haiku 4.5
	"claude-haiku-4.5": {InputPerM: 1.0, CachedPerM: 0.1, OutputPerM: 5.0, Source: "anthropic"},
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
	if p.InputPerM == 0 && p.OutputPerM == 0 && p.CachedPerM == 0 {
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
	// Strip common provider prefixes ("codex/", "kr/", "codezdev/", etc.)
	clean := stripModelPrefix(model)
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

// ComputeCostBreakdown returns the per-component USD cost of a request. If the
// model is unknown, all fields are zero. `cached` is the cache-hit token count
// (a subset of `input`); it is clamped to `input` if it exceeds it.
func ComputeCostBreakdown(model string, input, cached, output int) CostBreakdown {
	p, ok := LookupPricing(model)
	if !ok {
		return CostBreakdown{}
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

// EffectiveTokens returns the "real" tokens consumed = (input - cached) + output.
// This is the metric to display when the user wants to know how many tokens
// were actually processed (not served from cache).
func EffectiveTokens(input, cached, output int) int {
	if cached > input {
		cached = input
	}
	return (input - cached) + output
}
