package proxy

import (
	"testing"

	"omniproxy/config"
)

// TestRequestRecordCarriesDialect pins the contract: a usage record must carry
// the wire dialect the upstream was called with, and the By Dialect breakdown
// must aggregate it. Without this, an operator looking at Usage → By Dialect
// would see nothing for external accounts and could not tell which dialect
// their traffic actually used.
func TestRequestRecordCarriesDialect(t *testing.T) {
	initConfigForTests(t)
	account := config.Account{
		ID:                 "ext-dialect-rec",
		Email:              "dialect@example.com",
		AuthMethod:         externalAuthMethod,
		BaseURL:            "https://example.test",
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	tracker := &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	h := &Handler{usageTracker: tracker}

	h.recordUsageWithCache("", account.ID, "gpt-5", endpointOpenAIResponses, 100, 50, 0, cacheUsageTelemetry{})

	stats := tracker.GetStats("all")
	if len(stats.RecentRequests) != 1 {
		t.Fatalf("recent requests = %d, want 1", len(stats.RecentRequests))
	}
	rec := stats.RecentRequests[0]
	if rec.Dialect != "responses" {
		t.Fatalf("record.Dialect = %q, want responses", rec.Dialect)
	}
	if rec.Endpoint != endpointOpenAIResponses {
		t.Fatalf("record.Endpoint = %q, want %s", rec.Endpoint, endpointOpenAIResponses)
	}

	summary := stats.ByDialect["responses"]
	if summary == nil {
		t.Fatal("ByDialect[responses] is nil, want aggregated summary")
	}
	if summary.Requests != 1 {
		t.Fatalf("ByDialect[responses].Requests = %d, want 1", summary.Requests)
	}
	if summary.PromptTokens != 100 || summary.CompletionTokens != 50 {
		t.Fatalf("ByDialect[responses] tokens = in=%d out=%d, want in=100 out=50",
			summary.PromptTokens, summary.CompletionTokens)
	}
}

// TestRequestRecordOmitsDialectForNonExternalAccounts ensures native traffic
// does not produce a spurious empty-string bucket in By Dialect. The field is
// omitempty in JSON and addToSummaryMap skips empty keys, so the UI never sees
// a phantom row.
func TestRequestRecordOmitsDialectForNonExternalAccounts(t *testing.T) {
	initConfigForTests(t)
	account := config.Account{
		ID:         "native-no-dialect",
		Email:      "native@example.com",
		AuthMethod: "kiro",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	tracker := &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	h := &Handler{usageTracker: tracker}

	h.recordUsageWithCache("", account.ID, "claude-sonnet-4.5", endpointClaude, 80, 40, 0, cacheUsageTelemetry{})

	stats := tracker.GetStats("all")
	if len(stats.RecentRequests) != 1 {
		t.Fatalf("recent requests = %d, want 1", len(stats.RecentRequests))
	}
	if stats.RecentRequests[0].Dialect != "" {
		t.Fatalf("native record.Dialect = %q, want empty", stats.RecentRequests[0].Dialect)
	}
	if len(stats.ByDialect) != 0 {
		t.Fatalf("ByDialect has %d entries, want 0 for native-only traffic", len(stats.ByDialect))
	}
}

// TestRecordErrorCarriesDialect covers the failure path: a failed request must
// also tag its dialect so an operator can tell whether errors cluster on one
// dialect or are spread across several.
func TestRecordErrorCarriesDialect(t *testing.T) {
	initConfigForTests(t)
	account := config.Account{
		ID:                 "ext-dialect-err",
		Email:              "err@example.com",
		AuthMethod:         externalAuthMethod,
		BaseURL:            "https://example.test",
		AccessToken:        "sk-test",
		ExternalAPIDialect: "anthropic",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	tracker := &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	h := &Handler{usageTracker: tracker}

	h.recordError("", account.ID, "claude-opus-5", endpointClaude, "upstream 500")

	stats := tracker.GetStats("all")
	if len(stats.RecentRequests) != 1 {
		t.Fatalf("recent requests = %d, want 1", len(stats.RecentRequests))
	}
	rec := stats.RecentRequests[0]
	if rec.Dialect != "anthropic" {
		t.Fatalf("error record.Dialect = %q, want anthropic", rec.Dialect)
	}
	if rec.Status != statusError {
		t.Fatalf("error record.Status = %q, want error", rec.Status)
	}

	summary := stats.ByDialect["anthropic"]
	if summary == nil {
		t.Fatal("ByDialect[anthropic] is nil for error record")
	}
	if summary.Errors != 1 {
		t.Fatalf("ByDialect[anthropic].Errors = %d, want 1", summary.Errors)
	}
}
