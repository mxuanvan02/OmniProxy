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

func TestRewriteClaudeDesktopModelToSOTA(t *testing.T) {
	desktop := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	desktop.Header.Set(claudeDesktopIAPHeader, "1")
	plain := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	tests := []struct {
		name  string
		req   *http.Request
		model string
		want  string
	}{
		{name: "desktop opus maps to model-O", req: desktop, model: "claude-opus-5", want: "model-O"},
		{name: "desktop sonnet maps to model-T", req: desktop, model: "claude-sonnet-5", want: "model-T"},
		{name: "desktop opus is case-insensitive", req: desktop, model: "Claude-Opus-5", want: "model-O"},
		{name: "desktop haiku is unchanged", req: desktop, model: "claude-haiku-5", want: "claude-haiku-5"},
		{name: "desktop fable maps to model-S", req: desktop, model: "claude-fable-5", want: "model-S"},
		{name: "non-desktop fable is unchanged", req: plain, model: "claude-fable-5", want: "claude-fable-5"},
		{name: "desktop already-sota is unchanged", req: desktop, model: "model-O", want: "model-O"},
		{name: "non-desktop opus is unchanged", req: plain, model: "claude-opus-5", want: "claude-opus-5"},
		{name: "nil request is unchanged", req: nil, model: "claude-opus-5", want: "claude-opus-5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteClaudeDesktopModelToSOTA(tc.req, tc.model); got != tc.want {
				t.Fatalf("rewrite(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestClaudeDesktopOpusRoutesToSOTAAndEchoesPublicModel(t *testing.T) {
	initConfigForTests(t)

	var capturedModel, capturedEffort string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		capturedModel, _ = body["model"].(string)
		capturedEffort, _ = body["reasoning_effort"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	account := &config.Account{
		ID:          "desktop-sota",
		Email:       "desktop-sota",
		AuthMethod:  externalAuthMethod,
		AccessToken: "sota-key",
		BaseURL:     upstream.URL,
		Enabled:     true,
	}
	if err := config.AddAccount(*account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList(account.ID, []string{"model-O", "model-T", "claude-opus-5"})

	h := &Handler{
		pool:         p,
		promptCache:  newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: &UsageTracker{ringCap: 8, ring: make([]RequestRecord, 8), activeReqs: make(map[string]ActiveRequest), dailyData: make(map[string]*PeriodSummary)},
	}

	body := `{"model":"claude-opus-5","max_tokens":64,"stream":false,"thinking":{"type":"adaptive","effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(claudeDesktopIAPHeader, "1")
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if resp.Model != "claude-opus-5" {
		t.Fatalf("client model = %q, want claude-opus-5", resp.Model)
	}
	if capturedModel != "model-O" {
		t.Fatalf("upstream model = %q, want model-O", capturedModel)
	}
	if capturedEffort != "xhigh" {
		t.Fatalf("upstream reasoning_effort = %q, want xhigh", capturedEffort)
	}
}

func TestNonDesktopOpusDoesNotRewriteToSOTA(t *testing.T) {
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

	account := &config.Account{
		ID:          "plain-opus",
		Email:       "plain-opus",
		AuthMethod:  externalAuthMethod,
		AccessToken: "opus-key",
		BaseURL:     upstream.URL,
		Enabled:     true,
	}
	if err := config.AddAccount(*account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList(account.ID, []string{"claude-opus-5"})

	h := &Handler{
		pool:         p,
		promptCache:  newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: &UsageTracker{ringCap: 8, ring: make([]RequestRecord, 8), activeReqs: make(map[string]ActiveRequest), dailyData: make(map[string]*PeriodSummary)},
	}

	body := `{"model":"claude-opus-5","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if capturedModel != "claude-opus-5" {
		t.Fatalf("upstream model = %q, want claude-opus-5", capturedModel)
	}
}
