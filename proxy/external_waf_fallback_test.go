package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExternalChatRetriesWAF403WithCurlProfile(t *testing.T) {
	initConfigForTests(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("User-Agent") != externalCurlUserAgent {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "Your request was blocked. error code: 1010")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	account := &config.Account{ID: "waf", Email: "waf", AuthMethod: externalAuthMethod, BaseURL: server.URL, AccessToken: "sk-test", Enabled: true}
	var text strings.Builder
	payload := OpenAIToKiro(&OpenAIRequest{Model: "gpt-test", Messages: []OpenAIMessage{{Role: "user", Content: "ping"}}}, false)
	err := CallExternalOpenAI(context.Background(), account, payload, &KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }})
	if err != nil {
		t.Fatalf("WAF fallback failed: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("request count = %d, want SDK attempt plus one curl fallback", got)
	}
	if got := text.String(); got != "pong" {
		t.Fatalf("text = %q, want pong", got)
	}
}

func TestExternalChatDoesNotRetryCredential403(t *testing.T) {
	initConfigForTests(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid API key"}}`)
	}))
	defer server.Close()

	account := &config.Account{ID: "auth", Email: "auth", AuthMethod: externalAuthMethod, BaseURL: server.URL, AccessToken: "bad-key", Enabled: true}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "gpt-test", Messages: []OpenAIMessage{{Role: "user", Content: "ping"}}}, false)
	err := CallExternalOpenAI(context.Background(), account, payload, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected credential error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("credential 403 request count = %d, want no fallback", got)
	}
}

func TestExternalChatRetriesTransientAuthUnavailable(t *testing.T) {
	initConfigForTests(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt < 3 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Encountered invalidated oauth token for user, failing request","type":"authentication_error","code":"auth_unavailable"}}`)
			return
		}
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	account := &config.Account{ID: "transient-auth", Email: "transient-auth", AuthMethod: externalAuthMethod, BaseURL: server.URL, AccessToken: "sk-test", Enabled: true}
	var text strings.Builder
	payload := OpenAIToKiro(&OpenAIRequest{Model: "gpt-test", Messages: []OpenAIMessage{{Role: "user", Content: "ping"}}}, false)
	err := CallExternalOpenAI(context.Background(), account, payload, &KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }})
	if err != nil {
		t.Fatalf("transient auth retry failed: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("request count = %d, want initial request plus two retries", got)
	}
	if got := text.String(); got != "pong" {
		t.Fatalf("text = %q, want pong", got)
	}
}

func TestExternalChatDoesNotRetryCredential401(t *testing.T) {
	initConfigForTests(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer server.Close()

	account := &config.Account{ID: "bad-auth", Email: "bad-auth", AuthMethod: externalAuthMethod, BaseURL: server.URL, AccessToken: "bad-key", Enabled: true}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "gpt-test", Messages: []OpenAIMessage{{Role: "user", Content: "ping"}}}, false)
	err := CallExternalOpenAI(context.Background(), account, payload, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected credential error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("credential 401 request count = %d, want no retry", got)
	}
}

func TestExternalModelsRetriesWAF403WithCurlProfile(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("User-Agent") != externalCurlUserAgent {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "Your request was blocked.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-test","owned_by":"external"}]}`)
	}))
	defer server.Close()

	models, err := fetchExternalProviderModels(&config.Account{ID: "models", BaseURL: server.URL, AccessToken: "sk-test"})
	if err != nil {
		t.Fatalf("model WAF fallback failed: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if len(models) != 1 || models[0].ModelId != "gpt-test" {
		t.Fatalf("models = %#v", models)
	}
}
