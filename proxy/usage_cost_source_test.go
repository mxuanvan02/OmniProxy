package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// The provenance of a cost figure must be recorded, because the two possible
// sources are not equally true. A gateway's own price list is what that gateway
// charges; the built-in vendor table is a guess about what a reseller charged.
//
// The bug this pins: a claude-opus-4.8 turn through a gateway that publishes no
// price list displayed $9.3331 — exactly Anthropic's list price for its tokens
// (1717038 uncached x $5/M + 1284158 cached x $0.5/M + 4234 x $25/M) — with
// nothing marking it as an estimate. The number looked authoritative because it
// was precise, and precision is what made it convincing.
func TestAppendRecordsCostSourceFromGatewayWhenPublished(t *testing.T) {
	initConfigForTests(t)

	const acct = "acct-with-price-list"
	// Install a gateway price list for this account, which is what
	// refreshAccountPricing does from /api/pricing.
	accountPricingStore.Store(acct, map[string]ModelPricing{
		"gpt-test": {PerCallUSD: 0.01, Source: "provider"},
	})
	t.Cleanup(func() { accountPricingStore.Delete(acct) })

	tr := newTestUsageTracker()
	tr.Append(RequestRecord{
		AccountID:   acct,
		Model:       "gpt-test",
		InputTokens: 100,
		Status:      statusSuccess,
	})

	rec := tr.mostRecent(t)
	if rec.CostSource != CostSourceProvider {
		t.Errorf("CostSource = %q, want %q: the gateway published a price, so the "+
			"figure is the gateway's own rate", rec.CostSource, CostSourceProvider)
	}
	if rec.RealCost != 0.01 {
		t.Errorf("RealCost = %v, want 0.01 (the gateway's flat per-call price)", rec.RealCost)
	}
}

// The fallback case — and the one that was silently misleading.
func TestAppendRecordsCostSourceAsVendorEstimateOnFallback(t *testing.T) {
	initConfigForTests(t)

	const acct = "acct-without-price-list"
	// No entry in accountPricingStore: this is the JUSTWOKER/seekai shape, where
	// /api/pricing answers 401 so no gateway list is ever cached.

	tr := newTestUsageTracker()
	tr.Append(RequestRecord{
		AccountID:   acct,
		Model:       "claude-opus-4.8", // in the built-in vendor table
		InputTokens: 1_000_000,
		Status:      statusSuccess,
	})

	rec := tr.mostRecent(t)
	if rec.CostSource != CostSourceVendor {
		t.Errorf("CostSource = %q, want %q: no gateway price matched, so this is "+
			"the vendor list price standing in for an unknown reseller rate",
			rec.CostSource, CostSourceVendor)
	}
	if rec.RealCost <= 0 {
		t.Errorf("RealCost = %v, want a positive vendor-table figure", rec.RealCost)
	}
}

// An unknown model has no price at all. CostSource must stay empty rather than
// claiming "vendor", because nothing priced the request and the zero total means
// nothing.
func TestAppendLeavesCostSourceEmptyWhenNothingPricedIt(t *testing.T) {
	initConfigForTests(t)

	tr := newTestUsageTracker()
	tr.Append(RequestRecord{
		AccountID:   "acct-x",
		Model:       "no-such-model-anywhere",
		InputTokens: 1000,
		Status:      statusSuccess,
	})

	rec := tr.mostRecent(t)
	if rec.CostSource != "" {
		t.Errorf("CostSource = %q, want empty for an unpriced model", rec.CostSource)
	}
	if rec.RealCost != 0 {
		t.Errorf("RealCost = %v, want 0", rec.RealCost)
	}
}

// A caller that supplies its own RealCost also supplies its own CostSource, and
// Append must not overwrite it. This is the seam a future gateway-ledger
// reconciliation would use to record what was actually charged.
func TestAppendPreservesCallerSuppliedCostSource(t *testing.T) {
	initConfigForTests(t)

	tr := newTestUsageTracker()
	tr.Append(RequestRecord{
		AccountID:   "acct-y",
		Model:       "claude-opus-4.8",
		InputTokens: 1_000_000,
		RealCost:    1.25,
		CostSource:  "ledger",
		Status:      statusSuccess,
	})

	rec := tr.mostRecent(t)
	if rec.CostSource != "ledger" {
		t.Errorf("CostSource = %q, want the caller's %q to be preserved", rec.CostSource, "ledger")
	}
	if rec.RealCost != 1.25 {
		t.Errorf("RealCost = %v, want the caller's 1.25 preserved", rec.RealCost)
	}
}

// A failed turn must carry the upstream status so the operator can tell a 524
// (origin timed out) from a 429 (throttled) from a cut stream (no status at all).
// All three currently render identically as a red dot.
func TestRecordErrorCarriesHTTPStatus(t *testing.T) {
	initConfigForTests(t)

	h := &Handler{usageTracker: newTestUsageTracker()}
	cases := []struct {
		errMsg string
		want   int
	}{
		{`HTTP 524 from VSLLM: <!DOCTYPE html>`, 524},
		{`HTTP 429 from VSLLM: {"error":{"message":"请求过于频繁"}}`, 429},
		{`external SSE stream ended before a terminal finish_reason or [DONE]`, 0},
	}
	for _, tc := range cases {
		h.recordError("key1", "acct1", "qwen3.8-max-0902", endpointOpenAI, tc.errMsg)
		rec := h.usageTracker.mostRecent(t)
		if rec.Status != statusError {
			t.Errorf("Status = %q, want error", rec.Status)
		}
		if rec.HTTPStatus != tc.want {
			t.Errorf("err %q: HTTPStatus = %d, want %d", tc.errMsg[:28], rec.HTTPStatus, tc.want)
		}
	}
}

// A successful turn gets 200 and the measured attempt latency. Latency is per
// attempt, not per turn, so a retried request does not report the sum.
func TestRecordUsageWithMetaCarriesStatusAndLatency(t *testing.T) {
	initConfigForTests(t)

	tr := newTestUsageTracker()
	h := &Handler{usageTracker: tr}
	h.recordUsageWithMeta("key1", "acct1", "qwen3.8-max-0902", endpointOpenAI,
		100, 50, 0, cacheUsageTelemetry{}, requestMeta{LatencyMs: 2400})

	rec := tr.mostRecent(t)
	if rec.HTTPStatus != successStatus {
		t.Errorf("HTTPStatus = %d, want %d for a completed turn", rec.HTTPStatus, successStatus)
	}
	if rec.LatencyMs != 2400 {
		t.Errorf("LatencyMs = %d, want 2400", rec.LatencyMs)
	}
	if rec.Status != statusSuccess {
		t.Errorf("Status = %q, want success", rec.Status)
	}
}

// The legacy wrapper (service endpoints: image, video, search, embeddings) passes
// no metadata. It must still succeed and default the status to 200 rather than
// recording 0, since those paths only record on success.
func TestRecordUsageLegacyWrapperStillRecordsSuccess(t *testing.T) {
	initConfigForTests(t)

	tr := newTestUsageTracker()
	h := &Handler{usageTracker: tr}
	h.recordUsage("key1", "acct1", "gpt-image-1", endpointImage, 0, 0, 0.04, 0, 0, 0)

	rec := tr.mostRecent(t)
	if rec.HTTPStatus != successStatus {
		t.Errorf("HTTPStatus = %d, want %d (legacy path records only on success)",
			rec.HTTPStatus, successStatus)
	}
	if rec.LatencyMs != 0 {
		t.Errorf("LatencyMs = %d, want 0 (not measured on the legacy path)", rec.LatencyMs)
	}
}

// The new fields must actually cross the wire, or the frontend columns render
// empty no matter how correct the recording is. This is the check that would
// have caught the Details tab, whose Latency map was always empty.
func TestUsageRecordSerializesNewFields(t *testing.T) {
	rec := RequestRecord{
		Model:      "qwen3.8-max-0902",
		HTTPStatus: 524,
		LatencyMs:  2400,
		CostSource: CostSourceVendor,
		Status:     statusError,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]interface{}{
		"httpStatus": float64(524),
		"latencyMs":  float64(2400),
		"costSource": "vendor",
	} {
		got, ok := back[key]
		if !ok {
			t.Errorf("field %q missing from the serialized record; the UI cannot render it", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

// omitempty must not hide a real 200: the status column would show "—" for every
// successful request, which is worse than not having the column.
func TestHTTPStatus200IsSerialized(t *testing.T) {
	b, err := json.Marshal(RequestRecord{HTTPStatus: successStatus, Model: "m"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"httpStatus":200`) {
		t.Errorf("serialized record dropped httpStatus 200: %s", b)
	}
}

// newTestUsageTracker builds a tracker with the maps Append touches, without the
// global singleton, so tests cannot leak state into each other.
func newTestUsageTracker() *UsageTracker {
	return &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
}

// mostRecent returns the newest record in the ring, failing the test if empty.
func (t *UsageTracker) mostRecent(tb testing.TB) RequestRecord {
	tb.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := 0; i < t.ringCap; i++ {
		idx := (t.ringIdx - 1 - i + t.ringCap*2) % t.ringCap
		if t.ring[idx].Model != "" {
			return t.ring[idx]
		}
	}
	tb.Fatal("no record in the usage ring")
	return RequestRecord{}
}
