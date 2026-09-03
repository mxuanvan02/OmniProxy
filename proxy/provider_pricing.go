// Package proxy — reseller gateway price lists.
//
// A one-api/new-api gateway publishes its own price list at /api/pricing, and
// that list is what the account is actually billed against. It rarely matches
// the model vendor's rate: the same claude-opus-5 costs $0.30 per call on one
// gateway, $0.80 on another, and is token-metered on a third. Pricing every
// request off the vendor table therefore misreports spend for these accounts.
package proxy

import (
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"sync"
)

// oneAPIQuotaUSDPerM converts a one-api model_ratio into USD per 1M tokens.
// The gateway's internal unit is quota, with 500000 quota = $1 and
// quota = ratio x tokens, so a ratio of 1 prices 1M tokens at $2.
const oneAPIQuotaUSDPerM = 2.0

// accountPricingStore maps an account ID to its gateway's price list, keyed by
// lowercased model name. Each entry is replaced wholesale on refresh, never
// mutated, so readers need no lock beyond the sync.Map itself.
var accountPricingStore sync.Map // accountID → map[string]ModelPricing

// lookupAccountPricing returns the price this account's gateway charges for a
// model. The bool is false when the gateway published no list, or no entry for
// this model — callers then fall back to the vendor list price.
func lookupAccountPricing(accountID, model string) (ModelPricing, bool) {
	if accountID == "" || model == "" {
		return ModelPricing{}, false
	}
	v, ok := accountPricingStore.Load(accountID)
	if !ok {
		return ModelPricing{}, false
	}
	models := v.(map[string]ModelPricing)
	clean := strings.ToLower(stripModelPrefix(model))
	if p, ok := models[clean]; ok {
		return p, true
	}
	// Gateways advertise dash-form Claude IDs ("claude-opus-4-8") while the
	// request may carry the dot form OmniProxy normalises to.
	if p, ok := models[strings.ToLower(dotToDashClaudeVersion(clean))]; ok {
		return p, true
	}
	return ModelPricing{}, false
}

// oneAPIPricingEntry is one model's billing rule in a /api/pricing response.
type oneAPIPricingEntry struct {
	ModelName string `json:"model_name"`
	// QuotaType 0 bills per token via the ratios below; 1 bills a flat
	// ModelPrice per request and ignores token counts entirely.
	QuotaType       int     `json:"quota_type"`
	ModelRatio      float64 `json:"model_ratio"`
	ModelPrice      float64 `json:"model_price"`
	CompletionRatio float64 `json:"completion_ratio"`
	CacheRatio      float64 `json:"cache_ratio"`
}

// oneAPIPricingResponse is the /api/pricing shape published by one-api and its
// forks (new-api, veloera, and the gateways built on them).
type oneAPIPricingResponse struct {
	Data       []oneAPIPricingEntry `json:"data"`
	GroupRatio map[string]float64   `json:"group_ratio"`
	AutoGroups []string             `json:"auto_groups"`
}

// pricingGroupRatio picks the multiplier for the group this key bills under.
// The endpoint does not say which group the token belongs to, so the gateway's
// own default group is used, falling back to "default" at ratio 1.
func pricingGroupRatio(resp *oneAPIPricingResponse) (string, float64) {
	group := "default"
	for _, g := range resp.AutoGroups {
		if g = strings.TrimSpace(g); g != "" {
			group = g
			break
		}
	}
	if r, ok := resp.GroupRatio[group]; ok && r > 0 {
		return group, r
	}
	return group, 1.0
}

// oneAPIEntryPricing converts one gateway billing rule into ModelPricing. The
// bool is false for entries the gateway prices at zero, which carry no usable
// rate and must not mask the vendor list price.
func oneAPIEntryPricing(e oneAPIPricingEntry, groupRatio float64) (ModelPricing, bool) {
	notes := "Gateway /api/pricing"
	if e.QuotaType == 1 {
		if e.ModelPrice <= 0 {
			return ModelPricing{}, false
		}
		return ModelPricing{
			PerCallUSD: e.ModelPrice * groupRatio,
			Source:     "provider",
			Notes:      notes + ": flat per-call charge",
		}, true
	}
	input := e.ModelRatio * oneAPIQuotaUSDPerM * groupRatio
	if input <= 0 {
		return ModelPricing{}, false
	}
	// A completion ratio of 0 is "unset", not free output: one-api then bills
	// output at the input rate.
	output := input
	if e.CompletionRatio > 0 {
		output = input * e.CompletionRatio
	}
	// Likewise an unset cache ratio means no cache discount is configured.
	cached := input
	if e.CacheRatio > 0 {
		cached = input * e.CacheRatio
	}
	return ModelPricing{
		InputPerM:  input,
		CachedPerM: cached,
		OutputPerM: output,
		Source:     "provider",
		Notes:      notes + ": derived from model/completion/cache ratios",
	}, true
}

// refreshAccountPricing fetches the gateway's price list and caches it against
// the account. A gateway with no /api/pricing endpoint returns
// ErrExternalCreditsNotSupported, which callers treat as "nothing to cache"
// rather than a failure.
func refreshAccountPricing(account *config.Account) error {
	if account == nil || !isExternalAccount(account) {
		return ErrExternalCreditsNotSupported
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return ErrExternalCreditsNotSupported
	}
	var resp oneAPIPricingResponse
	if err := getProviderJSON(account, providerRootURL(baseURL)+"/api/pricing", &resp); err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		return ErrExternalCreditsNotSupported
	}
	group, groupRatio := pricingGroupRatio(&resp)
	models := make(map[string]ModelPricing, len(resp.Data))
	for _, e := range resp.Data {
		name := strings.ToLower(strings.TrimSpace(e.ModelName))
		if name == "" {
			continue
		}
		if p, ok := oneAPIEntryPricing(e, groupRatio); ok {
			models[name] = p
		}
	}
	if len(models) == 0 {
		return ErrExternalCreditsNotSupported
	}
	accountPricingStore.Store(account.ID, models)
	logger.Infof("[ProviderPricing] %s: cached %d model prices (group %q, ratio %g)",
		account.Email, len(models), group, groupRatio)
	return nil
}

// accountPricingSnapshot returns every cached gateway price list, keyed by
// account ID. The inner maps are the stored ones, which are never mutated after
// being stored, so no copy is needed.
func accountPricingSnapshot() map[string]map[string]ModelPricing {
	out := make(map[string]map[string]ModelPricing)
	accountPricingStore.Range(func(k, v interface{}) bool {
		id, ok := k.(string)
		if !ok {
			return true
		}
		if models, ok := v.(map[string]ModelPricing); ok {
			out[id] = models
		}
		return true
	})
	return out
}
