package proxy

import (
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"testing"
)

// TestRecordedErrorCarriesAccountIdentity locks in the failover-attribution fix.
//
// Regression: every terminal recordError call site passed an empty accountID, so
// a failed request landed in Usage → Recent Requests / request-details with an
// empty account column and provider "unknown". An operator could see THAT a
// request failed but not WHICH account produced the failure.
//
// The upstream here rejects with HTTP 400 (not content-blocked, not auth, not
// transient), so the handler exhausts its single eligible account and takes the
// terminal error path. The recorded RequestRecord must name that account.
func TestRecordedErrorCarriesAccountIdentity(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream rejected the request"}}`))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	const accountID = "failing-external-account"
	const nickname = "FAILING ROUTER LIO"
	const provider = "external_openai"
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Email:       "failing-router@example.test",
		Nickname:    nickname,
		Provider:    provider,
		AuthMethod:  externalAuthMethod,
		AccessToken: "test-key",
		BaseURL:     server.URL,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("add external account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	tracker := &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	h := &Handler{
		pool:         p,
		promptCache:  newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: tracker,
	}
	payload := &KiroPayload{OriginalModel: "claude-opus-5"}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "Say OK",
		ModelID: "claude-opus-5",
		Origin:  "AI_EDITOR",
	}

	recorder := httptest.NewRecorder()
	h.handleClaudeStream(recorder, payload, "claude-opus-5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	stats := tracker.GetStats("all")
	var errRecords []RequestRecord
	for _, rec := range stats.RecentRequests {
		if rec.Status == statusError {
			errRecords = append(errRecords, rec)
		}
	}
	if len(errRecords) == 0 {
		t.Fatalf("no error record appended; recent records = %+v", stats.RecentRequests)
	}

	rec := errRecords[0]
	if rec.AccountID != accountID {
		t.Errorf("error record AccountID = %q, want %q (failure must be attributable to an account)", rec.AccountID, accountID)
	}
	if rec.AccountName != nickname {
		t.Errorf("error record AccountName = %q, want %q", rec.AccountName, nickname)
	}
	if rec.Provider == "unknown" {
		t.Errorf("error record Provider = %q; an attributed account must resolve its provider", rec.Provider)
	}
	if rec.Error == "" {
		t.Error("error record carries no error message")
	}
	if strings.Contains(rec.Error, "test-key") {
		t.Error("error record leaked the account credential")
	}
}
