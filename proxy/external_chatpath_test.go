package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"testing"
)

// TestExternalChatPathOverride pins the per-account chat-path override. A
// gateway may block POST /v1/chat/completions at the edge while serving the
// same OpenAI-shaped payload on another path (measured on api.justwoker.icu:
// /v1/chat/completions -> Cloudflare 403 HTML, /v1/completions -> 200
// chat.completion). Without the override the account is unusable even though
// the credential and models are fine.
func TestExternalChatPathOverride(t *testing.T) {
	initConfigForTests(t)

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/v1/chat/completions" {
			// Mirror the observed edge behaviour: the canonical path is refused
			// with an HTML body, not a JSON error.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "<!DOCTYPE html><title>Attention Required! | Cloudflare</title>")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	account := &config.Account{
		ID:          "jw",
		Email:       "justwoker",
		AuthMethod:  "external_openai",
		BaseURL:     server.URL,
		AccessToken: "sk-test",
		ChatPath:    "/v1/completions",
		Enabled:     true,
	}

	var text strings.Builder
	callback := &KiroStreamCallback{
		OnText: func(s string, _ bool) { text.WriteString(s) },
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-opus-5",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)
	if err := CallExternalOpenAI(context.Background(), account, payload, callback); err != nil {
		t.Fatalf("CallExternalOpenAI with ChatPath override: %v", err)
	}
	if gotPath != "/v1/completions" {
		t.Errorf("upstream path = %q, want /v1/completions", gotPath)
	}
	if got := text.String(); got != "PONG" {
		t.Errorf("text = %q, want PONG", got)
	}
}

// TestExternalChatPathDefault pins that accounts without an override keep
// using the OpenAI-standard path.
func TestExternalChatPathDefault(t *testing.T) {
	if got := externalChatPath(nil); got != "/v1/chat/completions" {
		t.Errorf("nil account path = %q", got)
	}
	if got := externalChatPath(&config.Account{}); got != "/v1/chat/completions" {
		t.Errorf("empty override path = %q", got)
	}
	if got := externalChatPath(&config.Account{ChatPath: "v1/completions"}); got != "/v1/completions" {
		t.Errorf("path without leading slash = %q, want /v1/completions", got)
	}
}

// TestExternalOpenAIHeadersFingerprint pins the OpenAI-SDK identity headers.
// The resale gateways behind Cloudflare answer Go's default User-Agent with an
// HTML 403; a User-Agent alone is still refused, and only the combination with
// at least one x-stainless-* header is served. Dropping these headers silently
// breaks every such provider, so the header set is asserted explicitly.
func TestExternalOpenAIHeadersFingerprint(t *testing.T) {
	req, err := http.NewRequest("POST", "https://example.invalid/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	setExternalOpenAIHeaders(req, "sk-test", "text/event-stream")

	if got := req.Header.Get("User-Agent"); got != externalOpenAIUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, externalOpenAIUserAgent)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	for _, h := range []string{
		"x-stainless-lang", "x-stainless-package-version",
		"x-stainless-runtime", "x-stainless-runtime-version",
		"x-stainless-os", "x-stainless-arch",
	} {
		if req.Header.Get(h) == "" {
			t.Errorf("missing %s — Cloudflare bot-fight rules reject the request without it", h)
		}
	}
}

// TestChatPathSurvivesAccountJSON pins that the override round-trips through
// the config file, since it is written by hand / by the admin API.
func TestChatPathSurvivesAccountJSON(t *testing.T) {
	in := config.Account{ID: "a", AuthMethod: "external_openai", ChatPath: "/v1/completions"}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"chatPath":"/v1/completions"`) {
		t.Fatalf("chatPath not serialised: %s", blob)
	}
	var out config.Account
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	if out.ChatPath != "/v1/completions" {
		t.Errorf("round-trip ChatPath = %q", out.ChatPath)
	}
}
