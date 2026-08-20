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
		AccessToken: "***",
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
	if capturedHeaders.Get("Authorization") != "Bearer ***" {
		t.Errorf("Authorization = %q, want Bearer token", capturedHeaders.Get("Authorization"))
	}
	if capturedHeaders.Get("User-Agent") != agentRouterUserAgent {
		t.Errorf("missing or invalid User-Agent header: %s", capturedHeaders.Get("User-Agent"))
	}
	if capturedHeaders.Get("x-stainless-lang") != "js" {
		t.Errorf("x-stainless-lang = %q, want js", capturedHeaders.Get("x-stainless-lang"))
	}
}

func TestCallExternalAgentRouterFallsBackToNonStreamBeforeOutput(t *testing.T) {
	initConfigForTests(t)
	var streams []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		stream, _ := body["stream"].(bool)
		streams = append(streams, stream)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"temporary upstream failure\"}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`))
	}))
	defer server.Close()

	account := &config.Account{AuthMethod: "agentrouter", AccessToken: "***", BaseURL: server.URL}
	payload := &KiroPayload{OriginalModel: "claude-opus-4-8"}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	var text, stopReason string
	var inputTokens, outputTokens, outputSignals int
	err := CallExternalAgentRouter(account, payload, &KiroStreamCallback{
		OnOutput:     func() { outputSignals++ },
		OnText:       func(chunk string, _ bool) { text += chunk },
		OnStopReason: func(reason string) { stopReason = reason },
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
	})
	if err != nil {
		t.Fatalf("CallExternalAgentRouter failed: %v", err)
	}
	if text != "recovered" || stopReason != "end_turn" {
		t.Fatalf("fallback result text=%q stopReason=%q", text, stopReason)
	}
	if inputTokens != 7 || outputTokens != 1 || outputSignals != 1 {
		t.Fatalf("fallback metadata input=%d output=%d outputSignals=%d", inputTokens, outputTokens, outputSignals)
	}
	if len(streams) != 2 || !streams[0] || streams[1] {
		t.Fatalf("stream attempts = %v, want [true false]", streams)
	}
}

func TestCallExternalAgentRouterFallsBackOnUnterminatedSSEError(t *testing.T) {
	initConfigForTests(t)
	var streams []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		stream, _ := body["stream"].(bool)
		streams = append(streams, stream)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			// Deliberately omit the trailing newline. ReadString returns this
			// frame together with io.EOF.
			_, _ = w.Write([]byte(`data: {"error":{"message":"stream unsupported"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	account := &config.Account{AuthMethod: "agentrouter", AccessToken: "***", BaseURL: server.URL}
	payload := &KiroPayload{OriginalModel: "claude-opus-4-8"}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	var text string
	err := CallExternalAgentRouter(account, payload, &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
	})
	if err != nil {
		t.Fatalf("CallExternalAgentRouter failed: %v", err)
	}
	if text != "recovered" {
		t.Fatalf("fallback text = %q, want recovered", text)
	}
	if len(streams) != 2 || !streams[0] || streams[1] {
		t.Fatalf("stream attempts = %v, want [true false]", streams)
	}
}

func TestCallExternalAgentRouterDoesNotFallbackAfterMetadata(t *testing.T) {
	initConfigForTests(t)
	requests := 0
	cacheSignals := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"stream interrupted\"}}\n\n"))
	}))
	defer server.Close()

	account := &config.Account{AuthMethod: "agentrouter", AccessToken: "***", BaseURL: server.URL}
	payload := &KiroPayload{OriginalModel: "claude-opus-4-8"}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	err := CallExternalAgentRouter(account, payload, &KiroStreamCallback{
		OnCacheRead: func(tokens int) { cacheSignals += tokens },
	})
	if err == nil {
		t.Fatal("expected stream error after metadata")
	}
	if cacheSignals != 3 {
		t.Fatalf("cacheSignals = %d, want 3", cacheSignals)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (metadata-bearing attempts must not replay)", requests)
	}
}

func TestCallExternalAgentRouterDoesNotFallbackAfterOutput(t *testing.T) {
	initConfigForTests(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"stream interrupted\"}}\n\n"))
	}))
	defer server.Close()

	account := &config.Account{AuthMethod: "agentrouter", AccessToken: "***", BaseURL: server.URL}
	payload := &KiroPayload{OriginalModel: "claude-opus-4-8"}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	var text string
	err := CallExternalAgentRouter(account, payload, &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
	})
	if err == nil {
		t.Fatal("expected stream error after partial output")
	}
	if text != "partial" {
		t.Fatalf("text = %q, want partial", text)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (no unsafe replay)", requests)
	}
}

func TestCallExternalAgentRouterDoesNotFallbackOnEmptyTruncatedSSE(t *testing.T) {
	initConfigForTests(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
	}))
	defer server.Close()

	account := &config.Account{AuthMethod: "agentrouter", AccessToken: "***", BaseURL: server.URL}
	payload := &KiroPayload{OriginalModel: "claude-opus-4-8"}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	if err := CallExternalAgentRouter(account, payload, nil); err == nil {
		t.Fatal("expected truncated SSE error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (transport/parser errors must not fallback)", requests)
	}
}

func TestCallExternalAgentRouterDoesNotFallbackAfterToolDeltaWithNilCallback(t *testing.T) {
	initConfigForTests(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"stream interrupted\"}}\n\n"))
	}))
	defer server.Close()

	account := &config.Account{AuthMethod: "agentrouter", AccessToken: "***", BaseURL: server.URL}
	payload := &KiroPayload{OriginalModel: "claude-opus-4-8"}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	if err := CallExternalAgentRouter(account, payload, nil); err == nil {
		t.Fatal("expected stream error after tool delta")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (tool output must not replay)", requests)
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
