package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"omniproxy/config"
)

// A gateway billing per call (one-api quota_type 1) must not be priced off
// token counts: the flat charge replaces the per-token rates entirely.
func TestOneAPIEntryPricingFlatPerCall(t *testing.T) {
	p, ok := oneAPIEntryPricing(oneAPIPricingEntry{
		ModelName: "claude-opus-5", QuotaType: 1, ModelPrice: 0.3,
	}, 1.0)
	if !ok {
		t.Fatal("per-call entry was rejected")
	}
	if p.PerCallUSD != 0.3 {
		t.Fatalf("PerCallUSD = %v, want 0.3", p.PerCallUSD)
	}
	if p.InputPerM != 0 || p.OutputPerM != 0 {
		t.Fatalf("per-call entry must carry no token rates, got %+v", p)
	}
}

// The group ratio multiplies whatever the gateway quotes, so a group-2 key on
// VSLLM's list pays twice the advertised price.
func TestOneAPIEntryPricingAppliesGroupRatio(t *testing.T) {
	p, _ := oneAPIEntryPricing(oneAPIPricingEntry{
		ModelName: "gpt-5.6-sol", QuotaType: 1, ModelPrice: 0.015,
	}, 2.0)
	if p.PerCallUSD != 0.03 {
		t.Fatalf("PerCallUSD = %v, want 0.03", p.PerCallUSD)
	}
}

// SOTAMODEL's claude-opus-5 (ratio 2.5, completion 5, cache 0.1) is known to
// match Anthropic's published $5/$0.50/$25, which pins the quota→USD base.
func TestOneAPIEntryPricingDerivesTokenRatesFromRatios(t *testing.T) {
	p, ok := oneAPIEntryPricing(oneAPIPricingEntry{
		ModelName: "claude-opus-5", QuotaType: 0,
		ModelRatio: 2.5, CompletionRatio: 5, CacheRatio: 0.1,
	}, 1.0)
	if !ok {
		t.Fatal("token-metered entry was rejected")
	}
	if p.InputPerM != 5 || p.CachedPerM != 0.5 || p.OutputPerM != 25 {
		t.Fatalf("got %v/%v/%v, want 5/0.5/25", p.InputPerM, p.CachedPerM, p.OutputPerM)
	}
}

// An unset completion/cache ratio is "not configured", which one-api bills at
// the input rate — not free.
func TestOneAPIEntryPricingUnsetRatiosFallBackToInputRate(t *testing.T) {
	p, _ := oneAPIEntryPricing(oneAPIPricingEntry{
		ModelName: "glm-5.3", QuotaType: 0, ModelRatio: 1.5,
	}, 1.0)
	if p.OutputPerM != p.InputPerM || p.CachedPerM != p.InputPerM {
		t.Fatalf("unset ratios must fall back to the input rate, got %+v", p)
	}
}

// A zero-priced entry carries no usable rate and must not mask the vendor list
// price by caching a $0 override.
func TestOneAPIEntryPricingRejectsZeroRates(t *testing.T) {
	if _, ok := oneAPIEntryPricing(oneAPIPricingEntry{ModelName: "x", QuotaType: 1}, 1.0); ok {
		t.Error("zero per-call price was accepted")
	}
	if _, ok := oneAPIEntryPricing(oneAPIPricingEntry{ModelName: "x", QuotaType: 0}, 1.0); ok {
		t.Error("zero model ratio was accepted")
	}
}

// The gateway price must win over the vendor table for that account only, and
// every other account keeps paying the vendor rate.
func TestGatewayPriceOverridesVendorRateForThatAccountOnly(t *testing.T) {
	initConfigForTests(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pricing" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auto_groups":["default"],"group_ratio":{"default":1},
			"data":[{"model_name":"claude-opus-5","quota_type":1,"model_price":0.3}]}`))
	}))
	defer srv.Close()

	acc := &config.Account{
		ID: "gw-priced", Email: "gw@example.test",
		AuthMethod: externalAuthMethod, AccessToken: "k", BaseURL: srv.URL,
	}
	if err := refreshAccountPricing(acc); err != nil {
		t.Fatalf("refreshAccountPricing: %v", err)
	}
	defer accountPricingStore.Delete(acc.ID)

	if got := ResolveCredits(acc.ID, 0, "claude-opus-5", 500_000, 0, 10_000); got != 0.3 {
		t.Fatalf("gateway-priced credits = %v, want the flat 0.3", got)
	}
	vendor := ResolveCredits("other-account", 0, "claude-opus-5", 500_000, 0, 10_000)
	if vendor == 0.3 || vendor == 0 {
		t.Fatalf("other account = %v, want the vendor token-metered rate", vendor)
	}
}

// Gateways advertise dash-form Claude IDs while OmniProxy normalises requests
// to the dot form, so the lookup has to bridge the two spellings.
func TestLookupAccountPricingMatchesDashFormGatewayIDs(t *testing.T) {
	accountPricingStore.Store("dash-acct", map[string]ModelPricing{
		"claude-opus-4-8": {PerCallUSD: 0.025, Source: "provider"},
	})
	defer accountPricingStore.Delete("dash-acct")

	if _, ok := lookupAccountPricing("dash-acct", "claude-opus-4.8"); !ok {
		t.Fatal("dot-form request did not match the gateway's dash-form entry")
	}
}

// A gateway with no /api/pricing must report "nothing to cache" rather than an
// error, so a refresh does not surface a scary message for the common case.
func TestRefreshAccountPricingTreatsMissingEndpointAsUnsupported(t *testing.T) {
	initConfigForTests(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	err := refreshAccountPricing(&config.Account{
		ID: "no-pricing", AuthMethod: externalAuthMethod, AccessToken: "k", BaseURL: srv.URL,
	})
	if err != ErrExternalCreditsNotSupported {
		t.Fatalf("err = %v, want ErrExternalCreditsNotSupported", err)
	}
}
