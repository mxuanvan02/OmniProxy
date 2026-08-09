package proxy

import (
	"omniproxy/config"
	"sync/atomic"
	"testing"
	"time"
)

// TestUpdateTokensFromEventReadsUpstreamCounts verifies that when the upstream
// event carries a real usage map, updateTokensFromEvent extracts the exact
// input/output token counts instead of leaving the running estimate untouched.
func TestUpdateTokensFromEventReadsUpstreamCounts(t *testing.T) {
	cases := []struct {
		name     string
		event    map[string]interface{}
		startIn  int
		startOut int
		wantIn   int
		wantOut  int
	}{
		{
			name: "top-level camelCase",
			event: map[string]interface{}{
				"inputTokens":  float64(1234),
				"outputTokens": float64(56),
			},
			wantIn:  1234,
			wantOut: 56,
		},
		{
			name: "nested usage map snake_case",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"input_tokens":  float64(900),
					"output_tokens": float64(42),
				},
			},
			wantIn:  900,
			wantOut: 42,
		},
		{
			name: "cache components sum to input",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"uncachedInputTokens":   float64(100),
					"cacheReadInputTokens":  float64(800),
					"cacheWriteInputTokens": float64(50),
					"outputTokens":          float64(7),
				},
			},
			wantIn:  950,
			wantOut: 7,
		},
		{
			name: "total minus output yields input",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"totalTokens":  float64(1000),
					"outputTokens": float64(120),
				},
			},
			wantIn:  880,
			wantOut: 120,
		},
		{
			name:     "no usage shape keeps running values",
			event:    map[string]interface{}{"content": "hello"},
			startIn:  111,
			startOut: 22,
			wantIn:   111,
			wantOut:  22,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIn, gotOut := updateTokensFromEvent(tc.event, tc.startIn, tc.startOut)
			if gotIn != tc.wantIn {
				t.Fatalf("input tokens: got %d, want %d", gotIn, tc.wantIn)
			}
			if gotOut != tc.wantOut {
				t.Fatalf("output tokens: got %d, want %d", gotOut, tc.wantOut)
			}
		})
	}
}

// TestContextWindowForModelUsesUpstreamLimit verifies the percentage→token
// conversion uses the real per-model maxInputTokens from ListAvailableModels
// when cached, and falls back to the hard-coded window otherwise.
func TestContextWindowForModelUsesUpstreamLimit(t *testing.T) {
	h := &Handler{}

	// No cache: fall back to the version-based constant (4.8 → 1M).
	if got := h.contextWindowForModel("claude-opus-4.8"); got != 1_000_000 {
		t.Fatalf("fallback window for opus-4.8: got %d, want 1000000", got)
	}
	// 4.5 → 200K fallback.
	if got := h.contextWindowForModel("claude-sonnet-4.5"); got != 200_000 {
		t.Fatalf("fallback window for sonnet-4.5: got %d, want 200000", got)
	}

	// With an upstream-reported limit cached, prefer it over the constant.
	h.cachedModels = []ModelInfo{
		{ModelId: "claude-sonnet-4.5", TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 250000, MaxOutputTokens: 8192}},
	}
	if got := h.contextWindowForModel("claude-sonnet-4.5"); got != 250000 {
		t.Fatalf("cached window for sonnet-4.5: got %d, want 250000", got)
	}
	// Thinking suffix must resolve to the same base model's limit.
	if got := h.contextWindowForModel("claude-sonnet-4.5-thinking"); got != 250000 {
		t.Fatalf("cached window for sonnet-4.5-thinking: got %d, want 250000", got)
	}
	// A model not in the cache still falls back.
	if got := h.contextWindowForModel("claude-opus-4.8"); got != 1_000_000 {
		t.Fatalf("uncached model should fall back: got %d, want 1000000", got)
	}
}

// TestSumDailyTotalsNotCappedByRing verifies the headline totals come from the
// uncapped daily aggregation, so they keep growing past the ring buffer cap
// (the "stuck at 500" bug) instead of being counted from recent ring records.
func TestSumDailyTotalsNotCappedByRing(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	old := time.Now().UTC().AddDate(0, 0, -100).Format("2006-01-02")

	tr := &UsageTracker{
		ringCap: 500,
		ring:    make([]RequestRecord, 500),
		dailyData: map[string]*PeriodSummary{
			today:     {Requests: 4259, PromptTokens: 722756144, CompletionTokens: 2549452, Cost: 5557.5},
			yesterday: {Requests: 1236, PromptTokens: 147630238, CompletionTokens: 607684, Cost: 1176.9},
			old:       {Requests: 9999, PromptTokens: 1, CompletionTokens: 1, Cost: 99.0},
		},
	}

	// "all" sums every day, vastly exceeding the 500 ring cap.
	all := &UsageStats{}
	tr.sumDailyTotalsLocked(all, "all")
	if all.TotalRequests != 4259+1236+9999 {
		t.Fatalf("all-period total requests: got %d, want %d", all.TotalRequests, 4259+1236+9999)
	}
	if all.TotalRequests <= 500 {
		t.Fatalf("total must not be capped at ring size, got %d", all.TotalRequests)
	}

	// "7d" excludes the 100-day-old bucket but keeps today + yesterday.
	week := &UsageStats{}
	tr.sumDailyTotalsLocked(week, "7d")
	if week.TotalRequests != 4259+1236 {
		t.Fatalf("7d total requests: got %d, want %d", week.TotalRequests, 4259+1236)
	}

	// "today" keeps only the current UTC day.
	day := &UsageStats{}
	tr.sumDailyTotalsLocked(day, "today")
	if day.TotalRequests != 4259 {
		t.Fatalf("today total requests: got %d, want %d", day.TotalRequests, 4259)
	}
	if day.TotalCompletionTokens != 2549452 {
		t.Fatalf("today completion tokens: got %d, want %d", day.TotalCompletionTokens, 2549452)
	}

	// "24h" rolls back one UTC day, so it also includes yesterday's bucket —
	// it must NOT match "today" anymore.
	roll := &UsageStats{}
	tr.sumDailyTotalsLocked(roll, "24h")
	if roll.TotalRequests != 4259+1236 {
		t.Fatalf("24h total requests: got %d, want %d", roll.TotalRequests, 4259+1236)
	}
	if roll.TotalRequests == day.TotalRequests {
		t.Fatalf("24h must differ from today, both = %d", day.TotalRequests)
	}
}

func TestCacheHitTokensUsesEffectiveAggregate(t *testing.T) {
	// The cache sources are on different requests. max(cacheRead, cachedTokens)
	// would lose the OpenAI hit tokens after their daily aggregation.
	if got := cacheHitTokens(200, 20, 120, 90, 10); got != 100 {
		t.Fatalf("cache hits from effective aggregate: got %d, want 100", got)
	}

	// Malformed upstream cache counts cannot exceed their request's input total.
	if got := cacheHitTokens(100, 10, 1, 400, 0); got != 100 {
		t.Fatalf("cache hits must be clamped to input: got %d, want 100", got)
	}

	// Legacy aggregates without effective tokens retain the same safety bound.
	if got := cacheHitTokens(100, 0, 0, 150, 25); got != 100 {
		t.Fatalf("legacy cache hits must be clamped to input: got %d, want 100", got)
	}
}

// TestSumDailyBreakdownsNotCappedByRing verifies the By* breakdown tables are
// merged from the uncapped per-day buckets (not the 500-record ring), so a model
// or account that accumulated thousands of requests over many days reports its
// true lifetime totals, and the period cutoff still filters out old days.
func TestSumDailyBreakdownsNotCappedByRing(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	old := time.Now().UTC().AddDate(0, 0, -100).Format("2006-01-02")

	mk := func(reqs, prompt, completion int, cost float64) *PeriodSummary {
		return &PeriodSummary{Requests: reqs, PromptTokens: prompt, CompletionTokens: completion, Cost: cost}
	}

	tr := &UsageTracker{
		ringCap: 500,
		ring:    make([]RequestRecord, 500),
		dailyData: map[string]*PeriodSummary{
			today: {
				Requests: 4000, PromptTokens: 100, CompletionTokens: 10, Cost: 40,
				ByModel:   map[string]*PeriodSummary{"opus": mk(3000, 60, 6, 30), "sonnet": mk(1000, 40, 4, 10)},
				ByAccount: map[string]*PeriodSummary{"acc-1": mk(4000, 100, 10, 40)},
			},
			yesterday: {
				Requests: 1500, PromptTokens: 50, CompletionTokens: 5, Cost: 15,
				ByModel:   map[string]*PeriodSummary{"opus": mk(1500, 50, 5, 15)},
				ByAccount: map[string]*PeriodSummary{"acc-1": mk(1500, 50, 5, 15)},
			},
			old: {
				Requests: 9999, PromptTokens: 1, CompletionTokens: 1, Cost: 99,
				ByModel:   map[string]*PeriodSummary{"opus": mk(9999, 1, 1, 99)},
				ByAccount: map[string]*PeriodSummary{"acc-old": mk(9999, 1, 1, 99)},
			},
		},
	}

	newStats := func() *UsageStats {
		return &UsageStats{
			ByModel:    make(map[string]*PeriodSummary),
			ByAccount:  make(map[string]*PeriodSummary),
			ByAPIKey:   make(map[string]*PeriodSummary),
			ByEndpoint: make(map[string]*PeriodSummary),
		}
	}

	// "all": opus spans all three days and far exceeds the ring cap.
	all := newStats()
	tr.sumDailyTotalsLocked(all, "all")
	if got := all.ByModel["opus"].Requests; got != 3000+1500+9999 {
		t.Fatalf("all-period opus requests: got %d, want %d", got, 3000+1500+9999)
	}
	if all.ByModel["opus"].Requests <= 500 {
		t.Fatalf("breakdown must not be capped at ring size, got %d", all.ByModel["opus"].Requests)
	}
	if got := all.ByAccount["acc-1"].Requests; got != 4000+1500 {
		t.Fatalf("all-period acc-1 requests: got %d, want %d", got, 4000+1500)
	}

	// "7d": the 100-day-old bucket (and its acc-old / opus contribution) is excluded.
	week := newStats()
	tr.sumDailyTotalsLocked(week, "7d")
	if got := week.ByModel["opus"].Requests; got != 3000+1500 {
		t.Fatalf("7d opus requests: got %d, want %d", got, 3000+1500)
	}
	if _, ok := week.ByAccount["acc-old"]; ok {
		t.Fatal("7d must not include the 100-day-old account")
	}

	// "today": only the current UTC day's breakdown.
	day := newStats()
	tr.sumDailyTotalsLocked(day, "today")
	if got := day.ByModel["sonnet"].Requests; got != 1000 {
		t.Fatalf("today sonnet requests: got %d, want 1000", got)
	}
	if got := day.ByModel["opus"].Requests; got != 3000 {
		t.Fatalf("today opus requests: got %d, want 3000", got)
	}
}

// TestRecordErrorAppendsFailedRecord verifies a failed request is no longer
// invisible: recordError must bump the global failure counters AND append a
// RequestRecord with Status=statusError plus the reason, so the failure shows up
// in Usage → Recent Requests with its error message (previously recordFailure
// only bumped a counter and the failed request vanished from every table).
func TestRecordErrorAppendsFailedRecord(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	tr := &UsageTracker{
		ringCap:    500,
		ring:       make([]RequestRecord, 500),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	h := &Handler{usageTracker: tr}

	h.recordError("key-1", "", "claude-opus-4.8", endpointOpenAI, "upstream 500: boom")

	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("failedRequests: got %d, want 1", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("totalRequests: got %d, want 1", got)
	}

	stats := tr.GetStats("all")
	if len(stats.RecentRequests) != 1 {
		t.Fatalf("expected one recent request, got %d", len(stats.RecentRequests))
	}
	rec := stats.RecentRequests[0]
	if rec.Status != statusError {
		t.Fatalf("status: got %q, want %q", rec.Status, statusError)
	}
	if rec.Error != "upstream 500: boom" {
		t.Fatalf("error message not recorded, got %q", rec.Error)
	}
	if rec.Endpoint != endpointOpenAI {
		t.Fatalf("endpoint: got %q, want %q", rec.Endpoint, endpointOpenAI)
	}
	if rec.Model != "claude-opus-4.8" {
		t.Fatalf("model: got %q, want claude-opus-4.8", rec.Model)
	}
}

func TestTokensForAccountSinceUsesPersistedDailyAccountUsage(t *testing.T) {
	tracker := &UsageTracker{
		dailyData: map[string]*PeriodSummary{
			"2026-07-28": {
				ByAccount: map[string]*PeriodSummary{
					"codex-a": {PromptTokens: 500, CompletionTokens: 50},
				},
			},
			"2026-07-29": {
				ByAccount: map[string]*PeriodSummary{
					"codex-a": {PromptTokens: 1_200, CompletionTokens: 80},
					"codex-b": {PromptTokens: 9_999, CompletionTokens: 1},
				},
			},
			"2026-07-30": {
				ByAccount: map[string]*PeriodSummary{
					"codex-a": {PromptTokens: 2_000, CompletionTokens: 170},
				},
			},
		},
	}

	start := time.Date(2026, time.July, 29, 4, 0, 0, 0, time.UTC)
	if got := tracker.TokensForAccountSince("codex-a", start); got != 3_450 {
		t.Fatalf("window tokens = %d, want 3450", got)
	}
}
