package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"omniproxy/config"
	"omniproxy/logger"
)

// External requests are not truncated to a byte budget, so the log these tests
// cover is the only signal an operator gets that a gateway refused a request for
// its size. It is worth testing for two opposite reasons: missing a real size
// rejection leaves the decision to leave external uncut without evidence, and
// reporting a size rejection that never happened sends someone hunting for a
// limit that was not the problem.

func TestExternalPayloadSizeRejectedDetection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"413 standard phrase", 413, `{"error":"Request Entity Too Large"}`, true},
		// 413 says it by itself; requiring the words again would drop the one
		// unambiguous signal this feature exists to collect.
		{"413 empty body", 413, ``, true},
		{"400 context_length_exceeded", 400, `{"error":{"code":"context_length_exceeded"}}`, true},
		{"400 maximum context length", 400, `{"error":{"message":"This model's maximum context length is 128000 tokens"}}`, true},
		{"400 prompt too long", 400, `{"error":{"message":"prompt is too long: 210000 tokens"}}`, true},
		{"400 too many tokens", 400, `{"error":{"message":"too many tokens in request"}}`, true},
		{"400 exceed the maximum", 400, `{"error":{"message":"input exceeds the maximum allowed length"}}`, true},
		// Anthropic names the ceiling as "exceed context limit", with no "maximum".
		// The real message wraps max_tokens in backticks, which a raw string
		// literal cannot hold, so the fixture splices them in rather than
		// dropping them and testing a message Anthropic never sends.
		{"400 anthropic context limit", 400, `{"error":{"message":"input length and ` + "`max_tokens`" + ` exceed context limit: 205000 + 8192 > 200000"}}`, true},
		// Google names it as "exceeds the limit" and counts bytes, not tokens.
		{"400 google exceeds the limit", 400, `{"error":{"message":"Request payload size exceeds the limit: 20971520 bytes."}}`, true},
		// llama.cpp names it as the "available context size".
		{"400 llama.cpp context size", 400, `{"error":{"message":"the request exceeds the available context size, try increasing it"}}`, true},
		// The copula form: the noun phrases above ("request too large") do not
		// match it, and gateways emit both.
		{"400 copula is too large", 400, `{"error":{"message":"Your request is too large. Please try again with a smaller request."}}`, true},

		// The status alone must never be enough: a 400 is the commonest status on
		// this path and almost always means a malformed field, not a size limit.
		{"400 validation error", 400, `{"error":{"message":"Invalid value for 'temperature': must be <= 2"}}`, false},
		{"400 max_tokens validation", 400, `{"error":{"message":"max_tokens must be greater than 0"}}`, false},
		{"400 unspecified", 400, `{"error":{"message":"bad request"}}`, false},
		{"400 empty body", 400, ``, false},
		{"401 is not a size error", 401, `{"error":{"message":"context length exceeded"}}`, false},
		{"403 is not a size error", 403, `{"error":{"message":"payload too large"}}`, false},
		{"429 is not a size error", 429, `{"error":{"message":"request too large"}}`, false},
		{"500 is not a size error", 500, `request entity too large`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status}
			if got := externalPayloadSizeRejected(resp, []byte(tc.body)); got != tc.want {
				t.Fatalf("externalPayloadSizeRejected(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}

	if externalPayloadSizeRejected(nil, []byte("payload too large")) {
		t.Fatalf("a nil response must not be treated as a size rejection")
	}
}

func TestExternalPayloadModelMirrorsTheBuilderChain(t *testing.T) {
	// The chain must match what the chat builder puts on the wire, or the log
	// names a model the gateway never saw: requested ID, then the Kiro-mapped
	// ID, then the literal "auto" the builder sends when both are empty.
	if got := externalPayloadModel(&KiroPayload{OriginalModel: "requested-id"}); got != "requested-id" {
		t.Fatalf("got %q, want the requested model id", got)
	}

	fromState := &KiroPayload{}
	fromState.ConversationState.CurrentMessage.UserInputMessage.ModelID = "kiro-mapped-id"
	if got := externalPayloadModel(fromState); got != "kiro-mapped-id" {
		t.Fatalf("got %q, want the Kiro-mapped id as a fallback", got)
	}

	if got := externalPayloadModel(&KiroPayload{}); got != "auto" {
		t.Fatalf("got %q, want auto, the value the builder sends when nothing is set", got)
	}
	if got := externalPayloadModel(nil); got != "unknown" {
		t.Fatalf("got %q, want unknown for a nil payload", got)
	}
}

// sizeRejectingServer answers every request with a body the detector classifies
// as a size rejection.
func sizeRejectingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 128000 tokens"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func payloadLogAccount(t *testing.T, baseURL string) *config.Account {
	t.Helper()
	return &config.Account{
		ID:          "acc-size",
		Email:       "size@example.com",
		AuthMethod:  "external_openai",
		BaseURL:     baseURL,
		AccessToken: "sk-size-test",
	}
}

// payloadLogFilter captures only the lines this feature writes, so an unrelated
// warning emitted during the call cannot make a negative assertion pass.
type payloadLogFilter struct {
	mu    sync.Mutex
	lines []string
}

func capturePayloadLogs(t *testing.T) *payloadLogFilter {
	t.Helper()
	prev := logger.GetLevel()
	logger.SetLevel(logger.LevelWarn)
	t.Cleanup(func() { logger.SetLevel(prev) })

	f := &payloadLogFilter{}
	unsub := logger.Subscribe(func(line string) {
		if !strings.Contains(line, "[ExternalPayload]") {
			return
		}
		f.mu.Lock()
		f.lines = append(f.lines, line)
		f.mu.Unlock()
	})
	t.Cleanup(unsub)
	return f
}

func (f *payloadLogFilter) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func sizeLogPayload(t *testing.T) *KiroPayload {
	t.Helper()
	req := &ClaudeRequest{
		Model:    "claude-sonnet-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	payload := ClaudeToKiro(req, false)
	payload.OriginalModel = "upstream-model-id"
	return payload
}

// TestChatSizeRejectionIsLoggedAndFlowUnchanged covers the requirement in both
// directions at once: the line appears, and the caller still receives the
// upstream's own error rather than a rewritten one. A size log that swallowed or
// reworded the failure would be a behaviour change, not an observation.
func TestChatSizeRejectionIsLoggedAndFlowUnchanged(t *testing.T) {
	initConfigForTests(t)
	srv := sizeRejectingServer(t)
	account := payloadLogAccount(t, srv.URL)
	payload := sizeLogPayload(t)

	// Replicate what the adapter puts on the wire so the logged size can be
	// asserted exactly rather than merely as "some positive number".
	body, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body["stream"] = true
	body["stream_options"] = map[string]bool{"include_usage": true}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	logs := capturePayloadLogs(t)
	callErr := CallExternalOpenAI(context.Background(), account, payload, &KiroStreamCallback{})

	if callErr == nil {
		t.Fatalf("expected the upstream 400 to surface as an error")
	}
	// Flow unchanged: the status, account and body still reach the caller.
	if !strings.Contains(callErr.Error(), "HTTP 400") || !strings.Contains(callErr.Error(), account.Email) {
		t.Fatalf("upstream error was rewritten: %v", callErr)
	}
	if !strings.Contains(callErr.Error(), "context_length_exceeded") {
		t.Fatalf("upstream body was dropped from the error: %v", callErr)
	}

	lines := logs.all()
	if len(lines) != 1 {
		t.Fatalf("expected exactly one [ExternalPayload] line, got %d: %v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{
		"size-exceeded",
		"account=" + account.Email,
		"model=upstream-model-id",
		"dialect=chat",
		"bytes=" + strconv.Itoa(len(raw)),
		"status=400",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q:\n%s", want, line)
		}
	}
}

// TestOrdinaryRejectionIsNotLoggedAsSize is the false-positive guard: a 400 that
// names a parameter must not produce the line, or the log stops being evidence.
func TestOrdinaryRejectionIsNotLoggedAsSize(t *testing.T) {
	initConfigForTests(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid value for 'temperature': must be <= 2"}}`))
	}))
	t.Cleanup(srv.Close)

	account := payloadLogAccount(t, srv.URL)
	logs := capturePayloadLogs(t)

	callErr := CallExternalOpenAI(context.Background(), account, sizeLogPayload(t), &KiroStreamCallback{})
	if callErr == nil {
		t.Fatalf("expected the upstream 400 to surface as an error")
	}
	if lines := logs.all(); len(lines) != 0 {
		t.Fatalf("a parameter validation error was logged as a size rejection: %v", lines)
	}
}

// TestAgentRouterSizeRejectionIsLogged covers the dialect whose helper had to
// carry the payload through: callAgentRouterOpenAIRequest builds its own body and
// takes no payload, so the model reaching the log is evidence that the extra
// parameter is actually threaded rather than dropped.
func TestAgentRouterSizeRejectionIsLogged(t *testing.T) {
	initConfigForTests(t)
	srv := sizeRejectingServer(t)
	account := payloadLogAccount(t, srv.URL)
	account.AuthMethod = "agentrouter"
	payload := sizeLogPayload(t)

	logs := capturePayloadLogs(t)
	callErr := CallExternalAgentRouter(context.Background(), account, payload, &KiroStreamCallback{})
	if callErr == nil {
		t.Fatalf("expected the upstream 400 to surface as an error")
	}

	lines := logs.all()
	if len(lines) != 1 {
		t.Fatalf("expected exactly one [ExternalPayload] line, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{"dialect=agentrouter", "model=upstream-model-id", "status=400"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line missing %q:\n%s", want, lines[0])
		}
	}
	if !strings.Contains(lines[0], "bytes=") {
		t.Errorf("log line has no byte count:\n%s", lines[0])
	}
}
