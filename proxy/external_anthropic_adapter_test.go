package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
)

// The adapter is the only place the Messages dialect touches the wire, so the
// tests here assert the request as the gateway receives it and the callback as
// the client receives it — a translation that is correct in isolation still
// fails if the path, the headers or the framing are wrong.

type anthropicGateway struct {
	*httptest.Server
	path      string
	headers   http.Header
	body      map[string]interface{}
	rawBody   []byte
	callCount int
}

// newAnthropicGateway answers every request with the supplied status, content
// type and body, recording what it received.
func newAnthropicGateway(t *testing.T, status int, contentType, body string) *anthropicGateway {
	t.Helper()
	gw := &anthropicGateway{}
	gw.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw.callCount++
		gw.path = r.URL.Path
		gw.headers = r.Header.Clone()
		gw.rawBody, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(gw.rawBody, &gw.body)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(gw.Close)
	return gw
}

// anthropicAccount builds the external account the adapter is called with. The
// config is initialised here because the adapter resolves the account's proxy
// URL through it before opening the connection.
func anthropicAccount(t *testing.T, baseURL, dialect string) *config.Account {
	t.Helper()
	initConfigForTests(t)
	return &config.Account{
		Email:              "ext@example.com",
		AuthMethod:         externalAuthMethod,
		BaseURL:            baseURL,
		AccessToken:        "sk-test-key",
		ExternalAPIDialect: dialect,
	}
}

func TestCallExternalAnthropicSendsTheMessagesContract(t *testing.T) {
	gw := newAnthropicGateway(t, http.StatusOK, "text/event-stream", anthropicTextStream())
	payload := anthropicTestPayload()
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools = []KiroToolWrapper{
		anthropicToolWrapper("read_file", map[string]interface{}{
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
		}),
	}

	var cap anthropicCapture
	if err := CallExternalAnthropic(context.Background(), anthropicAccount(t, gw.URL, "anthropic"), payload, anthropicCallback(&cap)); err != nil {
		t.Fatalf("call: %v", err)
	}

	if gw.path != "/v1/messages" {
		t.Fatalf("path = %q, want the Messages default", gw.path)
	}
	// The key goes out both ways: the gateways in this pool disagree about which
	// header they authenticate.
	if got := gw.headers.Get("x-api-key"); got != "sk-test-key" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := gw.headers.Get("Authorization"); got != "Bearer sk-test-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := gw.headers.Get("anthropic-version"); got != externalAnthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", got, externalAnthropicVersion)
	}
	// The upstream is always asked to stream; the non-stream client path is
	// served by buffering through the callback.
	if gw.body["stream"] != true {
		t.Fatalf("stream = %v, want true", gw.body["stream"])
	}
	if gw.body["system"] != "You are a careful agent." {
		t.Fatalf("system = %v", gw.body["system"])
	}
	if _, present := gw.body["max_tokens"]; !present {
		t.Fatalf("max_tokens missing from %v", gw.body)
	}
	tools, _ := gw.body["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want the one tool the payload carried", gw.body["tools"])
	}
	tool, _ := tools[0].(map[string]interface{})
	if tool["name"] != "read_file" {
		t.Fatalf("tool = %v", tool)
	}
	if schema, _ := tool["input_schema"].(map[string]interface{}); schema["type"] != "object" {
		t.Fatalf("input_schema = %v, want an object root", tool["input_schema"])
	}
	// And the answer came back through the callback.
	if strings.Join(cap.text, "") != "Hello world" || cap.stop != "end_turn" {
		t.Fatalf("text = %v stop = %q", cap.text, cap.stop)
	}
}

// A gateway that nests the route needs the override, and a leading slash is
// optional because the value is composed into a URL.
func TestCallExternalAnthropicHonoursThePathOverride(t *testing.T) {
	for _, override := range []string{"/anthropic/v1/messages", "anthropic/v1/messages"} {
		gw := newAnthropicGateway(t, http.StatusOK, "text/event-stream", anthropicTextStream())
		account := anthropicAccount(t, gw.URL, "anthropic")
		account.AnthropicPath = override

		if err := CallExternalAnthropic(context.Background(), account, anthropicTestPayload(), &KiroStreamCallback{}); err != nil {
			t.Fatalf("call: %v", err)
		}
		if gw.path != "/anthropic/v1/messages" {
			t.Fatalf("path = %q for override %q", gw.path, override)
		}
	}
}

// An account that answers JSON ignores stream=true. The body is the same
// Messages object, so the same events have to come back out of it.
func TestCallExternalAnthropicParsesAJSONBody(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",` +
		`"content":[{"type":"text","text":"from json"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":2}}`
	gw := newAnthropicGateway(t, http.StatusOK, "application/json", body)

	var cap anthropicCapture
	if err := CallExternalAnthropic(context.Background(), anthropicAccount(t, gw.URL, "anthropic"), anthropicTestPayload(), anthropicCallback(&cap)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if strings.Join(cap.text, "") != "from json" {
		t.Fatalf("text = %v", cap.text)
	}
	if cap.stop != "end_turn" || cap.inTokens != 11 || cap.outTokens != 3 || cap.cacheRead != 2 {
		t.Fatalf("stop = %q usage = %d/%d cache = %d", cap.stop, cap.inTokens, cap.outTokens, cap.cacheRead)
	}
}

// An SSE body with no Content-Type is the common case on this pool. Peeking the
// first byte must not consume it, or a valid stream reads as empty.
func TestCallExternalAnthropicSniffsAnUnlabelledStream(t *testing.T) {
	gw := newAnthropicGateway(t, http.StatusOK, "", anthropicTextStream())

	var cap anthropicCapture
	if err := CallExternalAnthropic(context.Background(), anthropicAccount(t, gw.URL, "anthropic"), anthropicTestPayload(), anthropicCallback(&cap)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if strings.Join(cap.text, "") != "Hello world" {
		t.Fatalf("text = %v, want the stream read after the peek", cap.text)
	}
}

// A blank turn must fail so the pool can rotate accounts instead of closing the
// client's turn with an empty answer.
func TestCallExternalAnthropicReportsABlankTurn(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	gw := newAnthropicGateway(t, http.StatusOK, "text/event-stream", stream)

	err := CallExternalAnthropic(context.Background(), anthropicAccount(t, gw.URL, "anthropic"), anthropicTestPayload(), &KiroStreamCallback{})
	if err == nil {
		t.Fatalf("call succeeded, want an error for a turn with no output")
	}
	if !strings.Contains(err.Error(), "without assistant output") {
		t.Fatalf("error = %q", err)
	}
}

// The status has to stay in the message so the pool's auth handling can disable
// the account on 401, and no other dialect may be tried in its place.
func TestCallExternalAnthropicReturnsUpstreamErrorsUnchanged(t *testing.T) {
	gw := newAnthropicGateway(t, http.StatusUnauthorized, "application/json", `{"error":{"message":"invalid x-api-key"}}`)

	err := CallExternalAnthropic(context.Background(), anthropicAccount(t, gw.URL, "anthropic"), anthropicTestPayload(), &KiroStreamCallback{})
	if err == nil {
		t.Fatalf("call succeeded, want the upstream status surfaced")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("error = %q, want the status and the body", err)
	}
	if gw.callCount != 1 {
		t.Fatalf("calls = %d, want no fallback attempt on another dialect", gw.callCount)
	}
}

func TestCallExternalAnthropicRejectsAnIncompleteAccount(t *testing.T) {
	cases := map[string]*config.Account{
		"no base url": {Email: "a@b.c", AccessToken: "sk-x"},
		"no key":      {Email: "a@b.c", BaseURL: "https://example.com"},
	}
	for name, account := range cases {
		t.Run(name, func(t *testing.T) {
			if err := CallExternalAnthropic(context.Background(), account, anthropicTestPayload(), &KiroStreamCallback{}); err == nil {
				t.Fatalf("call succeeded, want an error")
			}
		})
	}
	if err := CallExternalAnthropic(context.Background(), nil, anthropicTestPayload(), &KiroStreamCallback{}); err == nil {
		t.Fatalf("a nil account was accepted")
	}
}

// The dialect switch in dispatchChat is what makes the adapter reachable at all;
// the arm order matters because AgentRouter accounts also satisfy
// isExternalAccount.
func TestDispatchChatRoutesTheAnthropicDialect(t *testing.T) {
	gw := newAnthropicGateway(t, http.StatusOK, "text/event-stream", anthropicTextStream())

	account := anthropicAccount(t, gw.URL, "anthropic")
	var cap anthropicCapture
	if err := dispatchChat(context.Background(), account, anthropicTestPayload(), anthropicCallback(&cap)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gw.path != "/v1/messages" {
		t.Fatalf("path = %q, want the request routed to the Messages adapter", gw.path)
	}

	// The same account on the chat dialect must not reach the Messages path.
	chatGW := newAnthropicGateway(t, http.StatusOK, "text/event-stream", "data: [DONE]\n")
	if err := dispatchChat(context.Background(), anthropicAccount(t, chatGW.URL, "chat"), anthropicTestPayload(), &KiroStreamCallback{}); err == nil {
		t.Fatalf("dispatch on the chat dialect succeeded, want the chat adapter's own error")
	}
	if chatGW.path == "/v1/messages" {
		t.Fatalf("a chat account was routed to the Messages adapter")
	}
}

// anthropicToolWrapper builds a tool as the translators attach it: an anonymous
// ToolSpecification with a JSON Schema carried as a decoded object.
func anthropicToolWrapper(name string, schema map[string]interface{}) KiroToolWrapper {
	var wrapper KiroToolWrapper
	wrapper.ToolSpecification.Name = name
	wrapper.ToolSpecification.Description = "a test tool"
	wrapper.ToolSpecification.InputSchema.JSON = schema
	return wrapper
}
