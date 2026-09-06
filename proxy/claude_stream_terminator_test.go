package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
	accountpool "omniproxy/pool"
)

// parseSSEEventNames extracts the ordered "event:" names from an SSE body.
func parseSSEEventNames(body string) []string {
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
		}
	}
	return names
}

func countEvent(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}

// TestClaudeStreamTerminatesAfterMidStreamFailure pins the bug behind
// "the assistant stopped mid-task on its own".
//
// When an upstream dies *after* the first bytes reached the client, the turn
// cannot be retried — the prefix is already visible, so replaying it would
// corrupt the response. That branch used to emit only an `error` event and
// return, leaving the Anthropic message open: the client had seen
// message_start and content blocks but never a terminator. Claude Code reads
// an open message on a closed connection as an interrupted turn and stops,
// which looks like the model quitting rather than an upstream failure.
//
// The OpenAI-side equivalent of this branch already sent `data: [DONE]`. Only
// the Claude branch lacked its terminator — an asymmetry, not a design choice.
func TestClaudeStreamTerminatesAfterMidStreamFailure(t *testing.T) {
	initConfigForTests(t)

	const model = "claude-opus-5"

	// An upstream that emits one visible token, then hangs up mid-stream.
	// Writing real content is what makes the response unretryable: the proxy
	// has already forwarded output to the client.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial answer\"},\"index\":0}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack and close so the client observes a truncated stream rather
		// than a clean EOF with a terminal chunk.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
		}
	}))
	defer server.Close()

	if err := config.AddAccount(config.Account{
		ID: "midstream-die", Email: "midstream-die", AuthMethod: externalAuthMethod,
		AccessToken: "test-key", BaseURL: server.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()

	h := &Handler{
		pool:          p,
		promptCache:   newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker:  GetUsageTracker(),
		catalogStatus: newCatalogStatusStore(),
	}

	payload := &KiroPayload{OriginalModel: model}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "do a long task", ModelID: model, Origin: "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, payload, model, false,
		claudeThinkingResponseOptions{}, 1, nil, "")

	names := parseSSEEventNames(rec.Body.String())
	if len(names) == 0 {
		t.Fatalf("no SSE events emitted at all; body=%q", rec.Body.String())
	}

	// The contract that matters: whatever else happens, a turn that opened a
	// message must close it. Without message_stop the client waits forever or
	// treats the turn as interrupted.
	if countEvent(names, "message_start") > 0 && countEvent(names, "message_stop") == 0 {
		t.Fatalf("message opened but never terminated; the client is left mid-message. events=%v", names)
	}

	// message_stop must be last: anything after it is outside the message.
	if countEvent(names, "message_stop") > 0 && names[len(names)-1] != "message_stop" {
		t.Fatalf("message_stop is not the final event; events=%v", names)
	}
}

// TestEndClaudeSSETurnWithErrorEmitsErrorThenStop tests the terminator helper
// directly, independently of the network path above.
//
// stop_reason is deliberately absent: Anthropic defines no value meaning
// "upstream died", and claiming end_turn would tell the client the turn
// finished cleanly — converting a visible failure into a silent truncation,
// the exact symptom this fix removes.
func TestEndClaudeSSETurnWithErrorEmitsErrorThenStop(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	h.endClaudeSSETurnWithError(rec, rec, "api_error", "upstream vanished mid-stream")

	body := rec.Body.String()
	names := parseSSEEventNames(body)

	if len(names) != 2 || names[0] != "error" || names[1] != "message_stop" {
		t.Fatalf("events = %v, want [error message_stop]", names)
	}
	if !strings.Contains(body, "upstream vanished mid-stream") {
		t.Fatal("the upstream failure reason was dropped; the operator cannot tell why the turn ended")
	}
	if strings.Contains(body, "stop_reason") {
		t.Fatal("stop_reason was sent: that tells the client the turn ended normally and invites it to ignore the error")
	}
}

// A failure *before* any output must not emit a bare message_stop: the client
// was never told a message started, so a terminator for it is malformed.
func TestNoStrayTerminatorWhenMessageNeverStarted(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	h.sendClaudeSSEError(rec, rec, "api_error", "no accounts available")

	names := parseSSEEventNames(rec.Body.String())
	if countEvent(names, "message_stop") != 0 {
		t.Fatalf("message_stop emitted for a message that never started; events=%v", names)
	}
	if len(names) != 1 || names[0] != "error" {
		t.Fatalf("events = %v, want exactly [error]", names)
	}
}
