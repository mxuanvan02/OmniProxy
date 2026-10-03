package proxy

import (
	"omniproxy/config"
	"testing"
)

// The credit figures the gateway does NOT put on the subscription row: the
// ceiling lives on the plan catalog (/api/subscription/plans -> total_amount in
// quota units) and the quota-per-USD rate lives on /api/status (quota_per_unit).
// joinPlanCredits is the pure function that folds both onto each subscription so
// the UI can show "780 / 1000 credit" instead of a bare percentage.
//
// Every number below is from a live VSLLM probe, not invented:
//   - quota_per_unit = 500000 (1 USD = 500000 quota)
//   - plan 13 "五小时后见": total_amount 5000000 quota, 5h window
//   - plan 5  "先试一下":   total_amount 500000 quota, weekly
//   - the console's own label for plan 25: "100 credit" over "500000" quota,
//     which pins 1 credit = 5000 quota = $0.01 (US cent).
func TestJoinPlanCreditsFillsCeilingAndUsed(t *testing.T) {
	plans := map[int]planCatalogEntry{
		13: {Title: "五小时后见", TotalAmount: 5000000, PriceUSD: 80, ResetSeconds: 18000},
		5:  {Title: "先试一下", TotalAmount: 500000, PriceUSD: 1, ResetSeconds: 0},
	}
	subs := []config.ExternalSubscription{
		{ID: 186673, PlanID: 13, UsedPercent: 78, WindowMinutes: 300},
		{ID: 196543, PlanID: 5, UsedPercent: 100, WindowMinutes: 10080},
	}
	quotaPerUnit := 500000.0

	joinPlanCredits(subs, plans, quotaPerUnit)

	// plan 13: 5000000 quota / 500000 = $10 = 1000 credit; 78% used = 780 credit
	got := subs[0]
	if got.CreditCeiling != 1000 {
		t.Errorf("plan 13 CreditCeiling = %v, want 1000 credit (5000000 quota / 500000 per USD * 100)", got.CreditCeiling)
	}
	if got.CreditUsed != 780 {
		t.Errorf("plan 13 CreditUsed = %v, want 780 (78%% of 1000)", got.CreditUsed)
	}
	if got.PlanTitle != "五小时后见" {
		t.Errorf("plan 13 PlanTitle = %q, want the catalog title", got.PlanTitle)
	}
	if got.PlanPriceUSD != 80 {
		t.Errorf("plan 13 PlanPriceUSD = %v, want 80", got.PlanPriceUSD)
	}

	// plan 5: 500000 quota = $1 = 100 credit; 100% used = 100 credit
	got5 := subs[1]
	if got5.CreditCeiling != 100 {
		t.Errorf("plan 5 CreditCeiling = %v, want 100", got5.CreditCeiling)
	}
	if got5.CreditUsed != 100 {
		t.Errorf("plan 5 CreditUsed = %v, want 100", got5.CreditUsed)
	}
}

// A plan the catalog does not list (expired, or a plan id the gateway added
// after we cached) must leave the credit fields zero so the renderer falls back
// to the percentage bar rather than printing a fabricated "0 / 0".
func TestJoinPlanCreditsUnknownPlanLeavesCreditZero(t *testing.T) {
	plans := map[int]planCatalogEntry{
		13: {Title: "五小时后见", TotalAmount: 5000000, ResetSeconds: 18000},
	}
	subs := []config.ExternalSubscription{
		{ID: 1, PlanID: 999, UsedPercent: 50},
	}
	joinPlanCredits(subs, plans, 500000)
	if subs[0].CreditCeiling != 0 || subs[0].CreditUsed != 0 || subs[0].PlanTitle != "" {
		t.Errorf("unknown plan got credit data: %+v, want zeros", subs[0])
	}
}

// A zero/nonsensical quota_per_unit must not divide by zero or emit a huge
// ceiling. The gateway could omit it; the join degrades to "no ceiling".
func TestJoinPlanCreditsGuardsZeroQuotaPerUnit(t *testing.T) {
	plans := map[int]planCatalogEntry{
		13: {Title: "x", TotalAmount: 5000000},
	}
	subs := []config.ExternalSubscription{{ID: 1, PlanID: 13, UsedPercent: 10}}
	joinPlanCredits(subs, plans, 0) // degenerate rate
	if subs[0].CreditCeiling != 0 {
		t.Errorf("ceiling = %v with quota_per_unit 0, want 0 (no divide-by-zero)", subs[0].CreditCeiling)
	}
}

// A plan whose total_amount is 0 (unlimited-style plan) must not produce a
// ceiling of 0 that reads as "exhausted"; UsedPercent still drives the bar.
func TestJoinPlanCreditsUnlimitedPlanHasNoCeiling(t *testing.T) {
	plans := map[int]planCatalogEntry{
		20: {Title: "unlimited", TotalAmount: 0},
	}
	subs := []config.ExternalSubscription{{ID: 1, PlanID: 20, UsedPercent: 30, Unlimited: true}}
	joinPlanCredits(subs, plans, 500000)
	if subs[0].CreditCeiling != 0 {
		t.Errorf("unlimited plan ceiling = %v, want 0", subs[0].CreditCeiling)
	}
	if subs[0].PlanTitle != "unlimited" {
		t.Errorf("title not joined for unlimited plan: %q", subs[0].PlanTitle)
	}
}

// Rounding: credit is displayed as a whole number of US cents in the console, so
// the ceiling rounds to the nearest cent rather than carrying float dust, while
// used is derived from the (already-rounded) percent against the exact ceiling.
func TestJoinPlanCreditsRoundsToCent(t *testing.T) {
	plans := map[int]planCatalogEntry{
		30: {Title: "odd", TotalAmount: 1234567}, // /500000*100 = 246.9134 credit
	}
	subs := []config.ExternalSubscription{{ID: 1, PlanID: 30, UsedPercent: 33}}
	joinPlanCredits(subs, plans, 500000)
	// 1234567/500000*100 = 246.9134 -> rounds to 246.91 (2dp), not integer-truncated
	if d := subs[0].CreditCeiling - 246.91; d > 0.01 || d < -0.01 {
		t.Errorf("ceiling = %v, want ~246.91 (2dp), got %v", subs[0].CreditCeiling, subs[0].CreditCeiling)
	}
	if d := subs[0].CreditUsed - 81.48; d > 0.02 || d < -0.02 {
		t.Errorf("used = %v, want ~81.48 (33%% of 246.91)", subs[0].CreditUsed)
	}
}

// parsePlanCatalog decodes the /api/subscription/plans envelope: data[].plan{}
// with total_amount (quota), quota_reset_custom_seconds, price_amount, title.
// The nesting (data -> []{plan}) mirrors the subscription envelope, and getting
// it wrong silently yields an empty catalog -> every plan "unknown".
func TestParsePlanCatalogShape(t *testing.T) {
	body := `{"success":true,"data":[
	  {"plan":{"id":13,"title":"五小时后见","total_amount":5000000,"price_amount":80,
	    "currency":"USD","quota_reset_period":"custom","quota_reset_custom_seconds":18000}},
	  {"plan":{"id":5,"title":"先试一下","total_amount":500000,"price_amount":1,
	    "currency":"USD","quota_reset_period":"weekly","quota_reset_custom_seconds":0}}
	]}`
	plans, err := parsePlanCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parsePlanCatalog: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	if plans[13].TotalAmount != 5000000 || plans[13].ResetSeconds != 18000 || plans[13].PriceUSD != 80 {
		t.Errorf("plan 13 = %+v, want total=5000000 reset=18000 price=80", plans[13])
	}
	if plans[5].Title != "先试一下" {
		t.Errorf("plan 5 title = %q", plans[5].Title)
	}
}

// A catalog response that is success:false (auth mismatch / admin-only) must be
// an error, not an empty catalog: empty would silently mark every plan unknown
// and drop all credit ceilings from the UI.
func TestParsePlanCatalogRejectsSuccessFalse(t *testing.T) {
	body := `{"success":false,"message":"Unauthorized, insufficient privileges","data":null}`
	if _, err := parsePlanCatalog([]byte(body)); err == nil {
		t.Fatal("success:false accepted as an empty catalog; want an error")
	}
}

// parseQuotaPerUnit pulls the credit divisor from /api/status without a second
// hardcoded 500000. The console's own localStorage carried it, but the API is
// authoritative and survives a redeploy of the frontend.
func TestParseQuotaPerUnit(t *testing.T) {
	body := `{"success":true,"data":{"quota_per_unit":500000,"display_in_currency":true,
	  "custom_currency_symbol":"¥","usd_exchange_rate":1}}`
	got, err := parseQuotaPerUnit([]byte(body))
	if err != nil {
		t.Fatalf("parseQuotaPerUnit: %v", err)
	}
	if got != 500000 {
		t.Errorf("quota_per_unit = %v, want 500000", got)
	}
}

// A missing or zero quota_per_unit returns 0 + an error so the caller keeps a
// previously known rate instead of dividing by zero.
func TestParseQuotaPerUnitMissing(t *testing.T) {
	if v, err := parseQuotaPerUnit([]byte(`{"success":true,"data":{"display_in_currency":true}}`)); err == nil {
		t.Errorf("missing quota_per_unit -> v=%v err=nil, want an error", v)
	}
}
