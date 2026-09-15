package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// A failed request must be counted as an error on the day bucket. The tokens it
// reported before failing are still summed, so Requests alone cannot distinguish
// a healthy account from a broken one.
func TestAddToSummaryMapCountsErrors(t *testing.T) {
	m := map[string]*PeriodSummary{}
	addToSummaryMap(m, "acct-1", RequestRecord{Status: statusError, InputTokens: 12})
	addToSummaryMap(m, "acct-1", RequestRecord{Status: statusSuccess, InputTokens: 30})

	got := m["acct-1"]
	if got == nil {
		t.Fatal("no summary written for acct-1")
	}
	if got.Requests != 2 {
		t.Fatalf("requests = %d, want 2", got.Requests)
	}
	if got.Errors != 1 {
		t.Fatalf("errors = %d, want 1", got.Errors)
	}
	if got.PromptTokens != 42 {
		t.Fatalf("promptTokens = %d, want 42 — the error path altered token accounting", got.PromptTokens)
	}
}

// The regression this field exists for. A multi-day period is served by folding
// several day buckets through mergeSummaryMapInto, so counting errors only in
// addToSummaryMap leaves 24h correct and 7d silently reporting zero.
func TestMergeSummaryMapIntoCarriesErrors(t *testing.T) {
	day1 := map[string]*PeriodSummary{}
	day2 := map[string]*PeriodSummary{}
	addToSummaryMap(day1, "acct-1", RequestRecord{Status: statusError})
	addToSummaryMap(day2, "acct-1", RequestRecord{Status: statusError})
	addToSummaryMap(day2, "acct-1", RequestRecord{Status: statusSuccess})

	period := map[string]*PeriodSummary{}
	mergeSummaryMapInto(period, day1)
	mergeSummaryMapInto(period, day2)

	got := period["acct-1"]
	if got == nil {
		t.Fatal("no summary written for acct-1")
	}
	if got.Requests != 3 {
		t.Fatalf("requests = %d, want 3", got.Requests)
	}
	if got.Errors != 2 {
		t.Fatalf("errors = %d, want 2 — mergeSummaryMapInto dropped the error count", got.Errors)
	}
}

// Errors is omitempty so the payload does not grow for the common all-success
// case. The frontend types it optional for this reason, and reads a missing key
// as zero.
func TestPeriodSummaryOmitsZeroErrors(t *testing.T) {
	data, err := json.Marshal(PeriodSummary{Requests: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "errors") {
		t.Fatalf("zero error count was serialised: %s", data)
	}

	data, err = json.Marshal(PeriodSummary{Requests: 1, Errors: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"errors":3`) {
		t.Fatalf("non-zero error count missing from %s", data)
	}
}
