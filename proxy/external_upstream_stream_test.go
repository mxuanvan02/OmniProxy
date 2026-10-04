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

// blankUpstreamSSE is the exact shape measured on api.justwoker.icu for
// claude-opus-4-8 (2026-10-03): HTTP 200, a well-formed SSE stream, every
// metadata field present — and not one character of assistant text. The
// gateway streams only when it has nothing to say, so the turn dies with
// "ended without assistant output" no matter how many accounts the pool
// retries. Answering the same body without stream returns the completion.
const blankUpstreamSSE = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":35,"total_tokens":39}}

data: [DONE]

`

const nonStreamCompletion = `{"id":"c1","object":"chat.completion","model":"claude-opus-4-8","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`

// newBlankStreamUpstream mimics a gateway that answers a streamed request with
// an empty SSE body and a non-streamed request with the real completion. It
// records what the caller actually asked for.
func newBlankStreamUpstream(t *testing.T, gotStream *bool, gotAccept *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream        *bool           `json:"stream"`
			StreamOptions json.RawMessage `json:"stream_options"`
		}
		if raw, err := io.ReadAll(r.Body); err == nil {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("upstream received unparseable body: %v", err)
			}
			// stream_options must not ride along on a non-streaming request:
			// it is meaningless there and a strict gateway may reject it.
			if body.Stream != nil && !*body.Stream && len(body.StreamOptions) > 0 {
				t.Errorf("non-streaming request still carried stream_options: %s", body.StreamOptions)
			}
		}
		if body.Stream != nil {
			*gotStream = *body.Stream
		}
		*gotAccept = r.Header.Get("Accept")

		if body.Stream != nil && *body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, blankUpstreamSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamCompletion)
	}))
}

func boolPtr(b bool) *bool { return &b }

// TestExternalUpstreamStreamOffRecoversBlankStream pins the fix: an account
// whose gateway streams nothing but metadata gets the answer through the
// non-streaming path instead of failing the turn.
func TestExternalUpstreamStreamOffRecoversBlankStream(t *testing.T) {
	initConfigForTests(t)

	// Control: with the default (streaming) the blank upstream kills the turn.
	var gotStream bool
	var gotAccept string
	server := newBlankStreamUpstream(t, &gotStream, &gotAccept)
	defer server.Close()

	base := config.Account{
		ID:          "jw",
		Email:       "justwoker",
		AuthMethod:  "external_openai",
		BaseURL:     server.URL,
		AccessToken: "sk-test",
		Enabled:     true,
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-opus-4-8",
		Messages: []OpenAIMessage{{Role: "user", Content: "Reply with exactly: PONG"}},
	}, false)

	streamingAccount := base
	var text strings.Builder
	if err := CallExternalOpenAI(context.Background(), &streamingAccount, payload,
		&KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }}); err == nil {
		t.Errorf("default streaming account against a blank-stream gateway: got nil error, want a blank-turn failure")
	} else if !strings.Contains(err.Error(), "without assistant output") {
		t.Errorf("default streaming error = %q, want it to name the blank turn", err)
	}
	if !gotStream {
		t.Errorf("default account sent stream=%v, want true", gotStream)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("default account Accept = %q, want text/event-stream", gotAccept)
	}

	// Fix: opt the same account out of upstream streaming.
	gotStream = true
	gotAccept = ""
	off := base
	off.ExternalUpstreamStream = boolPtr(false)
	text.Reset()
	if err := CallExternalOpenAI(context.Background(), &off, payload,
		&KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }}); err != nil {
		t.Fatalf("CallExternalOpenAI with ExternalUpstreamStream=false: %v", err)
	}
	if gotStream {
		t.Errorf("opted-out account sent stream=true, want false")
	}
	if gotAccept != "application/json" {
		t.Errorf("opted-out account Accept = %q, want application/json", gotAccept)
	}
	if got := text.String(); got != "PONG" {
		t.Errorf("text = %q, want PONG", got)
	}
}

// TestExternalUpstreamStreamOffAnthropicDialect pins the same opt-out on the
// Anthropic Messages dialect, which is the path that produced the reported
// "external anthropic SSE stream ended without assistant output" failure.
func TestExternalUpstreamStreamOffAnthropicDialect(t *testing.T) {
	initConfigForTests(t)

	var gotStream bool
	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream *bool `json:"stream"`
		}
		if raw, err := io.ReadAll(r.Body); err == nil {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("unparseable body: %v", err)
			}
		}
		if body.Stream != nil {
			gotStream = *body.Stream
		}
		gotAccept = r.Header.Get("Accept")

		if gotStream {
			// Blank Messages stream: usage only, no content blocks.
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":35}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"PONG"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1}}`)
	}))
	defer server.Close()

	account := &config.Account{
		ID:                     "jwc",
		Email:                  "justwoker-claude",
		AuthMethod:             "external_openai",
		BaseURL:                server.URL,
		AccessToken:            "sk-test",
		ExternalAPIDialect:     "anthropic",
		ExternalUpstreamStream: boolPtr(false),
		Enabled:                true,
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-opus-4-8",
		Messages: []OpenAIMessage{{Role: "user", Content: "Reply with exactly: PONG"}},
	}, false)

	var text strings.Builder
	if err := CallExternalAnthropic(context.Background(), account, payload,
		&KiroStreamCallback{OnText: func(s string, _ bool) { text.WriteString(s) }}); err != nil {
		t.Fatalf("CallExternalAnthropic with ExternalUpstreamStream=false: %v", err)
	}
	if gotStream {
		t.Errorf("opted-out account sent stream=true, want false")
	}
	if gotAccept != "application/json" {
		t.Errorf("opted-out account Accept = %q, want application/json", gotAccept)
	}
	if got := text.String(); got != "PONG" {
		t.Errorf("text = %q, want PONG", got)
	}
}

// TestExternalUpstreamStreamingDefault pins the precedence: unset and true both
// stream, only an explicit false opts out. A nil account must not panic.
func TestExternalUpstreamStreamingDefault(t *testing.T) {
	if !externalUpstreamStreaming(nil) {
		t.Error("nil account: want streaming true")
	}
	if !externalUpstreamStreaming(&config.Account{}) {
		t.Error("unset field: want streaming true (legacy behaviour)")
	}
	if !externalUpstreamStreaming(&config.Account{ExternalUpstreamStream: boolPtr(true)}) {
		t.Error("explicit true: want streaming true")
	}
	if externalUpstreamStreaming(&config.Account{ExternalUpstreamStream: boolPtr(false)}) {
		t.Error("explicit false: want streaming false")
	}
}

// TestExternalUpstreamStreamSurvivesAccountJSON pins the round-trip, since the
// field is written by hand or by the admin API.
func TestExternalUpstreamStreamSurvivesAccountJSON(t *testing.T) {
	in := config.Account{
		ID:                     "a",
		AuthMethod:             "external_openai",
		ExternalUpstreamStream: boolPtr(false),
	}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"externalUpstreamStream":false`) {
		t.Fatalf("externalUpstreamStream not serialised: %s", blob)
	}
	var out config.Account
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExternalUpstreamStream == nil || *out.ExternalUpstreamStream {
		t.Errorf("round-trip ExternalUpstreamStream = %v, want pointer to false", out.ExternalUpstreamStream)
	}

	// Omitted field must stay nil, not become false — otherwise every existing
	// account in every operator's config would silently lose streaming.
	var omitted config.Account
	if err := json.Unmarshal([]byte(`{"id":"b","authMethod":"external_openai"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.ExternalUpstreamStream != nil {
		t.Errorf("omitted field decoded as %v, want nil", *omitted.ExternalUpstreamStream)
	}
}
