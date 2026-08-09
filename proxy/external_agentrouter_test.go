package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"os"
	"testing"
)

func TestIsAgentRouterAccount(t *testing.T) {
	acc1 := &config.Account{AuthMethod: "agentrouter"}
	if !isAgentRouterAccount(acc1) {
		t.Errorf("expected true for AuthMethod agentrouter")
	}

	acc2 := &config.Account{AuthMethod: "external_agentrouter"}
	if !isAgentRouterAccount(acc2) {
		t.Errorf("expected true for AuthMethod external_agentrouter")
	}

	acc3 := &config.Account{AuthMethod: "external_openai"}
	if isAgentRouterAccount(acc3) {
		t.Errorf("expected false for AuthMethod external_openai")
	}
}

func TestCallExternalAgentRouterHeaders(t *testing.T) {
	initConfigForTests(t)
	var capturedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request path = %s, want /v1/chat/completions", r.URL.Path)
		}
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello from agentrouter\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	account := &config.Account{
		ID:          "test-agentrouter-1",
		Email:       "test@agentrouter.org",
		AuthMethod:  "agentrouter",
		AccessToken: "sk-testkey123",
		BaseURL:     server.URL + "/v1/messages",
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-4-6"
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	var chunks []string
	cb := &KiroStreamCallback{
		OnText: func(c string, isThinking bool) { chunks = append(chunks, c) },
	}

	err := CallExternalAgentRouter(account, payload, cb)
	if err != nil {
		t.Fatalf("CallExternalAgentRouter failed: %v", err)
	}

	if len(chunks) == 0 || chunks[0] != "hello from agentrouter" {
		t.Errorf("unexpected chunks: %v", chunks)
	}

	// Verify the live-validated OpenAI/Stainless fingerprint.
	if capturedHeaders.Get("Authorization") != "Bearer sk-testkey123" {
		t.Errorf("Authorization = %q, want Bearer token", capturedHeaders.Get("Authorization"))
	}
	if capturedHeaders.Get("User-Agent") != agentRouterUserAgent {
		t.Errorf("missing or invalid User-Agent header: %s", capturedHeaders.Get("User-Agent"))
	}
	if capturedHeaders.Get("x-stainless-lang") != "js" {
		t.Errorf("x-stainless-lang = %q, want js", capturedHeaders.Get("x-stainless-lang"))
	}
}

func TestCallAgentRouterTestUsesVerifiedProbe(t *testing.T) {
	initConfigForTests(t)
	var requestBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("request method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request path = %s, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe-key" {
			t.Errorf("Authorization = %q, want Bearer token", got)
		}
		if got := r.Header.Get("x-stainless-package-version"); got != agentRouterStainlessPackageVersion {
			t.Errorf("x-stainless-package-version = %q, want %q", got, agentRouterStainlessPackageVersion)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &requestBody); err != nil {
			t.Fatalf("decode request body %q: %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer server.Close()

	account := &config.Account{
		Email:       "agentrouter-probe",
		AuthMethod:  "agentrouter",
		AccessToken: "sk-probe-key",
		BaseURL:     server.URL,
	}
	reply, err := CallAgentRouterTest(account)
	if err != nil {
		t.Fatalf("CallAgentRouterTest failed: %v", err)
	}
	if reply != "OK" {
		t.Fatalf("reply = %q, want OK", reply)
	}

	if got := requestBody["model"]; got != agentRouterTestModel {
		t.Errorf("model = %#v, want %q", got, agentRouterTestModel)
	}
	if got := requestBody["max_tokens"]; got != float64(agentRouterTestMaxTokens) {
		t.Errorf("max_tokens = %#v, want %d", got, agentRouterTestMaxTokens)
	}
	if got := requestBody["stream"]; got != false {
		t.Errorf("stream = %#v, want false", got)
	}
	messages, ok := requestBody["messages"].([]interface{})
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one message", requestBody["messages"])
	}
	message, ok := messages[0].(map[string]interface{})
	if !ok {
		t.Fatalf("message = %#v, want object", messages[0])
	}
	if message["role"] != "user" || message["content"] != agentRouterTestPrompt {
		t.Errorf("message = %#v, want user/%q", message, agentRouterTestPrompt)
	}
}

func TestAgentRouterRootURL(t *testing.T) {
	for _, input := range []string{
		"https://agentrouter.org",
		"https://agentrouter.org/v1",
		"https://agentrouter.org/v1/messages",
		"https://agentrouter.org/v1/chat/completions",
	} {
		if got := agentRouterRootURL(input); got != "https://agentrouter.org" {
			t.Errorf("agentRouterRootURL(%q) = %q, want root", input, got)
		}
	}
}

func TestFetchAgentRouterModels(t *testing.T) {
	initConfigForTests(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{
				{"id": "claude-3-5-sonnet-20241022"},
				{"id": "gpt-4o"},
			},
		})
	}))
	defer server.Close()

	account := &config.Account{
		ID:          "test-agentrouter-2",
		AuthMethod:  "agentrouter",
		AccessToken: "sk-testkey123",
		BaseURL:     server.URL,
	}

	models, err := fetchAgentRouterModels(account)
	if err != nil {
		t.Fatalf("fetchAgentRouterModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ModelId != "claude-3-5-sonnet-20241022" {
		t.Errorf("unexpected model 0: %s", models[0].ModelId)
	}
}

func TestLiveAgentRouterIntegration(t *testing.T) {
	initConfigForTests(t)
	apiKey := os.Getenv("AGENTROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("set AGENTROUTER_API_KEY to run the live AgentRouter integration test")
	}
	account := &config.Account{
		ID:          "agentrouter-live-test",
		Email:       "agentrouter-account",
		AccessToken: apiKey,
		AuthMethod:  "agentrouter",
		BaseURL:     "https://ps.air-outer.com",
		Enabled:     true,
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-3-5-sonnet-20241022"
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "Say 'AgentRouter OK' in 3 words."

	var fullText string
	cb := &KiroStreamCallback{
		OnText: func(chunk string, isThinking bool) {
			fullText += chunk
		},
	}

	err := CallExternalAgentRouter(account, payload, cb)
	if err != nil {
		t.Fatalf("CallExternalAgentRouter live test failed: %v", err)
	}

	t.Logf("Live AgentRouter Response: %s", fullText)
	if fullText == "" {
		t.Errorf("expected non-empty response from AgentRouter")
	}
}
