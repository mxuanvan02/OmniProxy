package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
)

// newResponsesTestAccount points an external account at a test server with the
// Responses dialect selected. Every test below needs the same shape, and the
// BaseURL must be the live server URL rather than a fixture constant.
func newResponsesTestAccount(t *testing.T, srv *httptest.Server, id string) *config.Account {
	t.Helper()
	initDialectTestConfig(t)
	return &config.Account{
		ID:                 id,
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}
}

// collectText returns a callback that concatenates assistant text, plus a
// pointer to the builder it writes into.
func collectText() (*KiroStreamCallback, *strings.Builder) {
	var text strings.Builder
	callback := &KiroStreamCallback{
		OnText: func(s string, isThinking bool) {
			if !isThinking {
				text.WriteString(s)
			}
		},
	}
	return callback, &text
}

// TestCallExternalOpenAIResponsesSendsResponsesShape pins the wire contract: the
// Responses path, the OpenAI-SDK identity headers, and a Responses-shaped body.
//
// The negative assertions carry most of the weight. "messages" and
// "stream_options" are chat-completions vocabulary; a request carrying either
// means the chat builder was used and the dialect selection did nothing.
// "temperature" is the mirror image: the Codex dialect suppresses sampling
// parameters, so its presence proves externalResponsesOptions was applied rather
// than codexResponsesOptions.
func TestCallExternalOpenAIResponsesSendsResponsesShape(t *testing.T) {
	var gotPath, gotAuth, gotUA string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
	}))
	defer srv.Close()

	account := newResponsesTestAccount(t, srv, "ext-responses")
	callback, text := collectText()

	if err := CallExternalOpenAIResponses(context.Background(), account, responsesDialectPayload(), callback); err != nil {
		t.Fatalf("CallExternalOpenAIResponses: %v", err)
	}

	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "OpenAI/Python") {
		t.Fatalf("User-Agent = %q, want the OpenAI SDK identity", gotUA)
	}
	if _, ok := gotBody["messages"]; ok {
		t.Fatal("body carried chat-completions messages; the Responses shape was not used")
	}
	if _, ok := gotBody["input"]; !ok {
		t.Fatal("body has no input items")
	}
	if gotBody["stream"] != true {
		t.Fatalf("stream = %v, want true", gotBody["stream"])
	}
	if _, ok := gotBody["stream_options"]; ok {
		t.Fatal("stream_options has no meaning in the Responses API and must not be sent")
	}
	if _, ok := gotBody["temperature"]; !ok {
		t.Fatal("body dropped temperature; the Codex dialect's options were used instead of the external ones")
	}
	if text.String() != "pong" {
		t.Fatalf("callback text = %q, want %q", text.String(), "pong")
	}
}

// A Codex-style reseller commonly serves Responses behind a prefix rather than at
// /v1/responses, so the account override is the difference between the dialect
// working and returning 404.
func TestCallExternalOpenAIResponsesHonoursPathOverride(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n")
	}))
	defer srv.Close()

	account := newResponsesTestAccount(t, srv, "ext-responses-path")
	account.ResponsesPath = "/codex/responses"

	if err := CallExternalOpenAIResponses(context.Background(), account, responsesDialectPayload(), &KiroStreamCallback{}); err != nil {
		t.Fatalf("CallExternalOpenAIResponses: %v", err)
	}
	if gotPath != "/codex/responses" {
		t.Fatalf("upstream path = %q, want /codex/responses", gotPath)
	}
}

// Gateway ignored stream=true and answered a single JSON Responses object. The
// adapter must still deliver the text rather than erroring on the missing SSE.
// The response deliberately carries no Content-Type, which is what forces the
// adapter down the peek path — the case where reading resp.Body after Peek
// instead of the buffered reader would silently drop the first byte.
func TestCallExternalOpenAIResponsesJSONFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello json"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	defer srv.Close()

	account := newResponsesTestAccount(t, srv, "ext-responses-json")
	callback, text := collectText()

	if err := CallExternalOpenAIResponses(context.Background(), account, responsesDialectPayload(), callback); err != nil {
		t.Fatalf("CallExternalOpenAIResponses: %v", err)
	}
	if !strings.Contains(text.String(), "hello json") {
		t.Fatalf("callback text = %q, want it to contain %q", text.String(), "hello json")
	}
}

// A 404 on the Responses path means the dialect is misconfigured. The adapter
// must surface it instead of quietly retrying as chat: a silent fallback would
// hide the misconfiguration and make the token accounting this dialect exists
// for meaningless.
func TestCallExternalOpenAIResponsesDoesNotFallBackToChat(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		http.Error(w, "<html>404</html>", http.StatusNotFound)
	}))
	defer srv.Close()

	account := newResponsesTestAccount(t, srv, "ext-responses-404")

	err := CallExternalOpenAIResponses(context.Background(), account, responsesDialectPayload(), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should name the status: %v", err)
	}
	for _, p := range hits {
		if p == "/v1/chat/completions" {
			t.Fatal("adapter fell back to the chat path; a misconfigured dialect must surface")
		}
	}
}

// A gateway that answers 200 and then closes the stream before
// response.completed has produced a truncated turn, not an empty answer. When
// real output was already streamed the parser recovers the partial turn so SVG
// tests and long generations do not surface as hard failures.
func TestCallExternalOpenAIResponsesRecoversTruncatedStreamWithOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		// No response.completed: the stream just ends here.
	}))
	defer srv.Close()

	account := newResponsesTestAccount(t, srv, "ext-responses-truncated")
	callback, text := collectText()

	err := CallExternalOpenAIResponses(context.Background(), account, responsesDialectPayload(), callback)
	if err != nil {
		t.Fatalf("expected recovery for truncated stream with output: %v", err)
	}
	if !strings.Contains(text.String(), "partial") {
		t.Fatalf("callback text = %q, want it to contain the delivered delta", text.String())
	}
}

// TestCallExternalOpenAIResponsesRequiresCredentials covers the two account
// fields the adapter cannot invent. A gateway with no base URL or no key must
// fail here with a message naming the account, not with an opaque transport
// error after a request to a malformed URL.
func TestCallExternalOpenAIResponsesRequiresCredentials(t *testing.T) {
	initDialectTestConfig(t)
	cases := []struct {
		name    string
		account *config.Account
		want    string
	}{
		{"no base url", &config.Account{ID: "no-url", AuthMethod: "external_openai", AccessToken: "sk-test"}, "baseUrl"},
		{"no api key", &config.Account{ID: "no-key", AuthMethod: "external_openai", BaseURL: "https://example.invalid"}, "apiKey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CallExternalOpenAIResponses(context.Background(), tc.account, responsesDialectPayload(), &KiroStreamCallback{})
			if err == nil {
				t.Fatalf("expected an error naming the missing %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the missing %s", err, tc.want)
			}
		})
	}
}
