package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"testing"
)

func TestIsClaudeDesktopRequestBlankHeader(t *testing.T) {
	blank := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	blank.Header.Set(claudeDesktopIAPHeader, "   ")
	if isClaudeDesktopRequest(blank) {
		t.Fatal("whitespace-only header must not count as Desktop")
	}
	missing := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if isClaudeDesktopRequest(missing) {
		t.Fatal("absent header must not count as Desktop")
	}
}

// newSOTATestHandler wires a single external account whose upstream is srv and
// whose discovered catalog is models.
func newSOTATestHandler(t *testing.T, id, srvURL string, models []string) *Handler {
	t.Helper()
	if err := config.AddAccount(config.Account{
		ID:          id,
		Email:       id,
		AuthMethod:  externalAuthMethod,
		AccessToken: id + "-key",
		BaseURL:     srvURL,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList(id, models)
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: &UsageTracker{
			ringCap:    8,
			ring:       make([]RequestRecord, 8),
			activeReqs: make(map[string]ActiveRequest),
			dailyData:  make(map[string]*PeriodSummary),
		},
	}
}

// TestClaudeDesktopSonnetStreamRoutesToSOTA covers the streaming path plus the
// Desktop-only "[1m]" context suffix: routing must land on model-T while
// message_start still echoes the picker ID the client asked for.
func TestClaudeDesktopSonnetStreamRoutesToSOTA(t *testing.T) {
	initConfigForTests(t)

	var capturedModel, capturedEffort string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		capturedModel, _ = body["model"].(string)
		capturedEffort, _ = body["reasoning_effort"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	h := newSOTATestHandler(t, "desktop-sonnet", upstream.URL, []string{"model-T", "claude-sonnet-5"})

	body := `{"model":"claude-sonnet-5[1m]","max_tokens":64,"stream":true,"thinking":{"type":"adaptive","effort":"max"},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(claudeDesktopIAPHeader, "1")
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if capturedModel != "model-T" {
		t.Fatalf("upstream model = %q, want model-T", capturedModel)
	}
	if capturedEffort != "max" {
		t.Fatalf("upstream reasoning_effort = %q, want max", capturedEffort)
	}

	events := parseClaudeSSEEvents(t, rec.Body.String())
	if len(events) == 0 || events[0].name != "message_start" {
		t.Fatalf("first event = %+v, want message_start", events)
	}
	message, _ := events[0].data["message"].(map[string]interface{})
	if got, _ := message["model"].(string); got != "claude-sonnet-5" {
		t.Fatalf("message_start model = %q, want claude-sonnet-5", got)
	}
}

// TestClaudeDesktopThinkingSuffixStillRewrites guards the ordering between
// thinking-suffix stripping and the SOTA rewrite.
func TestClaudeDesktopThinkingSuffixStillRewrites(t *testing.T) {
	initConfigForTests(t)

	var capturedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		capturedModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	h := newSOTATestHandler(t, "desktop-thinking", upstream.URL, []string{"model-O", "claude-opus-5"})

	body := `{"model":"claude-opus-5-thinking","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(claudeDesktopIAPHeader, "1")
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if capturedModel != "model-O" {
		t.Fatalf("upstream model = %q, want model-O", capturedModel)
	}
	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "claude-opus-5" {
		t.Fatalf("client model = %q, want claude-opus-5", resp.Model)
	}
}

func TestClaudeDesktopCountTokensSucceeds(t *testing.T) {
	initConfigForTests(t)

	h := &Handler{pool: accountpool.GetPool(), promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(claudeDesktopIAPHeader, "1")
	rec := httptest.NewRecorder()
	h.handleCountTokens(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if out["input_tokens"] < 1 {
		t.Fatalf("input_tokens = %d, want >= 1", out["input_tokens"])
	}
}
