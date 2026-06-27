package proxy

import (
	"testing"
	"time"
)

// TestUpdateTokensFromEventReadsUpstreamCounts verifies that when the upstream
// event carries a real usage map, updateTokensFromEvent extracts the exact
// input/output token counts instead of leaving the running estimate untouched.
func TestUpdateTokensFromEventReadsUpstreamCounts(t *testing.T) {
	cases := []struct {
		name      string
		event     map[string]interface{}
		startIn   int
		startOut  int
		wantIn    int
		wantOut   int
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
		ringCap:   500,
		ring:      make([]RequestRecord, 500),
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
