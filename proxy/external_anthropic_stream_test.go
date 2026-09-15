package proxy

import (
	"errors"
	"strings"
	"testing"
)

// The Messages API stream is a state machine: text arrives as deltas, a tool
// call arrives whole only once its block stops, and usage is split across
// message_start and message_delta. The parser has to reassemble all of that, so
// each event family is asserted against the shape the adapter emits.

// anthropicCapture records what a parser emitted, in order.
type anthropicCapture struct {
	text      []string
	thinking  []string
	toolCalls []KiroToolUse
	outputs   int
	stop      string
	inTokens  int
	outTokens int
	cacheRead int
	cacheNew  int
	completed bool
}

func anthropicCallback(cap *anthropicCapture) *KiroStreamCallback {
	return &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				cap.thinking = append(cap.thinking, text)
				return
			}
			cap.text = append(cap.text, text)
		},
		OnToolUse:     func(toolUse KiroToolUse) { cap.toolCalls = append(cap.toolCalls, toolUse) },
		OnOutput:      func() { cap.outputs++ },
		OnStopReason:  func(reason string) { cap.stop = reason },
		OnComplete:    func(in, out int) { cap.inTokens, cap.outTokens, cap.completed = in, out, true },
		OnCacheRead:   func(n int) { cap.cacheRead = n },
		OnCacheCreate: func(n int) { cap.cacheNew = n },
	}
}

func runAnthropicSSE(t *testing.T, stream string) (anthropicCapture, error) {
	t.Helper()
	var cap anthropicCapture
	err := parseExternalAnthropicSSE(strings.NewReader(stream), anthropicCallback(&cap))
	return cap, err
}

func anthropicTextStream() string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":30,"cache_creation_input_tokens":7}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
}

func TestAnthropicSSEReassemblesTextAndUsage(t *testing.T) {
	cap, err := runAnthropicSSE(t, anthropicTextStream())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(cap.text, ""); got != "Hello world" {
		t.Fatalf("text = %q, want the deltas joined", got)
	}
	if cap.stop != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", cap.stop)
	}
	if !cap.completed || cap.inTokens != 120 || cap.outTokens != 9 {
		t.Fatalf("usage = in %d out %d completed %v", cap.inTokens, cap.outTokens, cap.completed)
	}
	// Cache usage is reported separately because the Claude adapter maps it onto
	// its own cache_read/cache_creation counters.
	if cap.cacheRead != 30 || cap.cacheNew != 7 {
		t.Fatalf("cache usage = read %d create %d", cap.cacheRead, cap.cacheNew)
	}
	if cap.outputs == 0 {
		t.Fatalf("OnOutput was never called for a turn that produced text")
	}
}

func TestAnthropicSSEEmitsToolCallAtBlockStop(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"read_file"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	cap, err := runAnthropicSSE(t, stream)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cap.toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (only complete once the block stops)", len(cap.toolCalls))
	}
	call := cap.toolCalls[0]
	if call.ToolUseID != "toolu_9" || call.Name != "read_file" {
		t.Fatalf("tool call = %+v", call)
	}
	if call.Input["path"] != "a.txt" {
		t.Fatalf("tool input = %+v, want the fragments reassembled", call.Input)
	}
	if cap.stop != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", cap.stop)
	}
}

// A gateway that truncates the argument JSON still has to reach the client with
// something it can read, rather than a dropped call.
func TestAnthropicSSEKeepsUnparsableToolArguments(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	cap, err := runAnthropicSSE(t, stream)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cap.toolCalls) != 1 || cap.toolCalls[0].Input["_raw"] != `{"path":` {
		t.Fatalf("tool calls = %+v, want the raw arguments kept", cap.toolCalls)
	}
}

func TestAnthropicSSESeparatesThinkingFromText(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing options"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	cap, err := runAnthropicSSE(t, stream)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(cap.thinking, "") != "weighing options" {
		t.Fatalf("thinking = %v", cap.thinking)
	}
	if strings.Join(cap.text, "") != "the answer" {
		t.Fatalf("text = %v, want only the text block", cap.text)
	}
}

// An error object can arrive inside an HTTP 200 stream. It must surface as a
// provider error rather than as an empty but successful turn.
func TestAnthropicSSEErrorEventBecomesProviderError(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	}, "\n")

	_, err := runAnthropicSSE(t, stream)
	var providerErr *externalSSEProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %v, want externalSSEProviderError", err)
	}
	if !strings.Contains(providerErr.message, "overloaded_error") || !strings.Contains(providerErr.message, "Overloaded") {
		t.Fatalf("message = %q, want both the type and the text", providerErr.message)
	}
	// No output had been produced, so the caller may still replay the attempt.
	if providerErr.outputObserved {
		t.Fatalf("outputObserved = true, want false for an error before any content")
	}
}

func TestAnthropicSSERejectsIncompleteStreams(t *testing.T) {
	cases := map[string]struct {
		stream string
		substr string
		// emitted is what already reached the client before the failure. A
		// truncated stream forwards the deltas it received — the caller detects
		// that via OnOutput and stops retrying — while a blank turn emits nothing
		// at all, which is what keeps it retryable.
		emitted string
	}{
		"no message_stop": {
			stream:  `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			substr:  "before message_stop",
			emitted: "partial",
		},
		// A well-formed stream that carries nothing renderable is reported so the
		// caller can rotate accounts instead of closing the turn empty.
		"no assistant output": {
			stream: strings.Join([]string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
				`data: {"type":"message_stop"}`,
			}, "\n"),
			substr: "without assistant output",
		},
		"whitespace only": {
			stream: strings.Join([]string{
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"   "}}`,
				`data: {"type":"message_stop"}`,
			}, "\n"),
			substr: "blank assistant output",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cap, err := runAnthropicSSE(t, tc.stream)
			if err == nil {
				t.Fatalf("parse succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.substr)
			}
			if got := strings.Join(cap.text, ""); got != tc.emitted {
				t.Fatalf("emitted text = %q, want %q", got, tc.emitted)
			}
		})
	}
}

// Some gateways close the stream with the OpenAI sentinel. Treating it as an
// unknown event would drop a turn that had already been fully delivered.
func TestAnthropicSSEToleratesDoneSentinel(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
		`data: [DONE]`,
	}, "\n")

	cap, err := runAnthropicSSE(t, stream)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(cap.text, "") != "done" || cap.stop != "max_tokens" {
		t.Fatalf("text = %v stop = %q", cap.text, cap.stop)
	}
}

// A non-streaming body carries the failure in the same envelope, and it is
// reported as a provider error for the same reason the streaming one is.
func TestAnthropicJSONErrorEnvelopeBecomesProviderError(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"}}`

	var cap anthropicCapture
	err := parseExternalAnthropicJSON(strings.NewReader(body), anthropicCallback(&cap))
	var providerErr *externalSSEProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %v, want externalSSEProviderError", err)
	}
	if !strings.Contains(providerErr.message, "max_tokens") {
		t.Fatalf("message = %q, want the upstream text", providerErr.message)
	}
	if strings.Join(cap.text, "") != "" {
		t.Fatalf("emitted text %v for an error body", cap.text)
	}
}

// A JSON body carrying only whitespace text is the non-streaming form of a blank
// turn, and has to be reported the same way.
func TestAnthropicJSONBlankBodyIsRejected(t *testing.T) {
	body := `{"role":"assistant","content":[{"type":"text","text":"   "}],"stop_reason":"end_turn"}`

	if err := parseExternalAnthropicJSON(strings.NewReader(body), &KiroStreamCallback{}); err == nil {
		t.Fatalf("a blank turn was accepted")
	}
}

func TestAnthropicStopReasonVocabulary(t *testing.T) {
	cases := map[string]string{
		"":              "end_turn",
		"end_turn":      "end_turn",
		"stop_sequence": "end_turn",
		"max_tokens":    "max_tokens",
		"tool_use":      "tool_use",
	}
	for input, want := range cases {
		if got := normalizeAnthropicStopReason(input); got != want {
			t.Fatalf("normalizeAnthropicStopReason(%q) = %q, want %q", input, got, want)
		}
	}
}
