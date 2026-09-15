package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The translators no longer shape history for Kiro: sanitizeKiroHistory and
// truncatePayloadToLimit are applied by prepareKiroPayload at dispatch, because
// only there is the destination account known, and an external account accepts
// the full structured tool history that Kiro rejects. These tests cover that
// function directly — it is now the only place the Kiro constraint lives, and
// the tests in the translator package that used to cover it had to be pointed
// here to avoid passing vacuously.

// clonedByJSON deep-copies through JSON. KiroToolUse.Input is a
// map[string]interface{}, so this is not a way to build a wire-faithful copy —
// it is only a way to snapshot one for comparison.
func clonedByJSON(t *testing.T, payload *KiroPayload) *KiroPayload {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out KiroPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &out
}

func toolCycleRequest(cycles int) *ClaudeRequest {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < cycles; i++ {
		id := "t" + string(rune('0'+i))
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": id, "name": "exec_command", "input": map[string]interface{}{"cmd": "step"}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": id, "content": "OUT_" + id},
			}},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "now summarize"})
	return &ClaudeRequest{
		Model:    "claude-opus-4.8",
		Tools:    []ClaudeTool{{Name: "exec_command", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: msgs,
	}
}

// TestPrepareKiroPayloadLeavesInputUntouched is the guard for the account
// rotation loop: one payload pointer is reused across attempts, and the pool
// holds both Kiro and external accounts. If shaping wrote through to the
// caller's payload, a Kiro attempt followed by an external attempt in the same
// request would send the external gateway a history already flattened for Kiro.
func TestPrepareKiroPayloadLeavesInputUntouched(t *testing.T) {
	original := ClaudeToKiro(toolCycleRequest(3), false)
	before := clonedByJSON(t, original)

	shaped := prepareKiroPayload(original)

	after := clonedByJSON(t, original)
	beforeRaw, _ := json.Marshal(before)
	afterRaw, _ := json.Marshal(after)
	if !bytes.Equal(beforeRaw, afterRaw) {
		t.Fatalf("prepareKiroPayload mutated its input\nbefore: %s\nafter:  %s", beforeRaw, afterRaw)
	}

	// Guard against the test passing because nothing happened at all.
	shapedRaw, _ := json.Marshal(clonedByJSON(t, shaped))
	if bytes.Equal(beforeRaw, shapedRaw) {
		t.Fatalf("prepareKiroPayload returned an unshaped payload; the isolation check above is vacuous")
	}
	if shaped == original {
		t.Fatalf("prepareKiroPayload returned the caller's own pointer")
	}
}

// TestPrepareKiroPayloadFlattensHistoryForKiro asserts the constraint still
// applies where it must: no history entry reaching Kiro carries structured tool
// activity, and the tool output survives as text.
func TestPrepareKiroPayloadFlattensHistoryForKiro(t *testing.T) {
	shaped := prepareKiroPayload(ClaudeToKiro(toolCycleRequest(3), false))

	var historyText strings.Builder
	for i, h := range shaped.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if len(a.ToolUses) > 0 {
				t.Fatalf("history[%d] still carries %d structured toolUses", i, len(a.ToolUses))
			}
			historyText.WriteString(a.Content)
			historyText.WriteString("\n")
		}
		if u := h.UserInputMessage; u != nil {
			if u.UserInputMessageContext != nil {
				if n := len(u.UserInputMessageContext.ToolResults); n > 0 {
					t.Fatalf("history[%d] still carries %d structured toolResults", i, n)
				}
				if n := len(u.UserInputMessageContext.Tools); n > 0 {
					t.Fatalf("history[%d] still carries %d structured tools", i, n)
				}
			}
			historyText.WriteString(u.Content)
			historyText.WriteString("\n")
		}
	}

	combined := historyText.String()
	for i := 0; i < 3; i++ {
		id := "t" + string(rune('0'+i))
		if !strings.Contains(combined, "OUT_"+id) {
			t.Errorf("tool output %q was lost instead of narrated", "OUT_"+id)
		}
	}
}

// padRequest prepends plain user/assistant turns big enough to push the
// translated payload past maxPayloadBytes. The padding is deliberately plain
// text: sanitize leaves it alone, so the payload stays oversized however
// sanitize and truncate are ordered, and the test measures the order rather
// than the shrink.
func padRequest(req *ClaudeRequest, turns, size int) *ClaudeRequest {
	chunk := strings.Repeat("x", size)
	padding := make([]ClaudeMessage, 0, turns*2)
	for i := 0; i < turns; i++ {
		padding = append(padding,
			ClaudeMessage{Role: "user", Content: chunk},
			ClaudeMessage{Role: "assistant", Content: chunk},
		)
	}
	req.Messages = append(padding, req.Messages...)
	return req
}

// TestPrepareKiroPayloadTruncatesAfterSanitizing pins the order. Sanitize
// shrinks a tool-heavy history enough to change whether truncation fires at all,
// so running truncate first would insert a placeholder into payloads that never
// needed one — and change payloadCacheKey, repinning a different account.
func TestPrepareKiroPayloadTruncatesAfterSanitizing(t *testing.T) {
	req := padRequest(toolCycleRequest(3), 30, 40_000)

	plain := ClaudeToKiro(req, false)
	rawSize := payloadByteSize(plain)

	shaped := prepareKiroPayload(plain)

	if rawSize <= maxPayloadBytes {
		t.Fatalf("fixture is %d bytes, at or under the %d limit, so truncation was never exercised",
			rawSize, maxPayloadBytes)
	}
	if payloadByteSize(shaped) > maxPayloadBytes {
		t.Fatalf("shaped payload is %d bytes, over the %d limit", payloadByteSize(shaped), maxPayloadBytes)
	}

	var placeholders int
	for _, h := range shaped.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, truncationPlaceholder[:20]) {
			placeholders++
		}
	}
	if placeholders == 0 {
		t.Fatalf("expected a truncation placeholder once the payload exceeded the limit")
	}

	// The current message is never dropped, whatever else is trimmed.
	if !strings.Contains(shaped.ConversationState.CurrentMessage.UserInputMessage.Content, "now summarize") {
		t.Fatalf("current message lost to truncation: %q",
			shaped.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

// TestPrepareKiroPayloadConsultsPrimingFlag proves truncation reads the flag the
// translator recorded rather than guessing. The priming pair has to survive as a
// unit; if it is mistaken for ordinary conversation, dropLeadingAssistant can
// take the assistant half and strand the system prompt.
func TestPrepareKiroPayloadConsultsPrimingFlag(t *testing.T) {
	req := padRequest(toolCycleRequest(3), 30, 40_000)
	req.System = "You are a helpful agent."

	// One translation, so both runs below see the same AgentContinuationId and
	// the only thing that differs is the flag. Translating twice would compare
	// two different uuids and pass without the flag mattering at all.
	base := ClaudeToKiro(req, false)
	if payloadByteSize(base) <= maxPayloadBytes {
		t.Fatalf("fixture is not oversized, so truncation never consults the flag")
	}

	base.hasPriming = true
	withPriming := prepareKiroPayload(base)
	if len(withPriming.ConversationState.History) == 0 {
		t.Fatalf("expected history to survive")
	}
	firstUser := withPriming.ConversationState.History[0].UserInputMessage
	if firstUser == nil || !strings.Contains(firstUser.Content, "helpful agent") {
		t.Fatalf("priming was not preserved at the head of history; got %+v", firstUser)
	}

	base.hasPriming = false
	withoutPriming := prepareKiroPayload(base)

	withRaw, _ := json.Marshal(withPriming)
	withoutRaw, _ := json.Marshal(withoutPriming)
	if bytes.Equal(withRaw, withoutRaw) {
		t.Fatalf("clearing hasPriming changed nothing, so truncation does not consult it")
	}
}

// TestCloneHistoryForSanitizeIsolatesCallerPayload covers the clone directly, at
// the level where the aliasing bug would live: the message structs sanitize
// writes through must not be the caller's.
func TestCloneHistoryForSanitizeIsolatesCallerPayload(t *testing.T) {
	history := []KiroHistoryMessage{
		asstWithTool("t1"),
		userWithResults("t1"),
	}
	clone := cloneHistoryForSanitize(history)

	if &history[0] == &clone[0] {
		t.Fatalf("clone shares the history slice backing array")
	}
	if history[0].AssistantResponseMessage == clone[0].AssistantResponseMessage {
		t.Fatalf("clone shares the assistant message struct")
	}
	if history[1].UserInputMessage == clone[1].UserInputMessage {
		t.Fatalf("clone shares the user message struct")
	}
	if history[1].UserInputMessage.UserInputMessageContext == clone[1].UserInputMessage.UserInputMessageContext {
		t.Fatalf("clone shares the user input context, so nilling ToolResults would reach the caller")
	}

	// Mutating the clone through the same paths sanitize uses must leave the
	// original intact.
	clone[0].AssistantResponseMessage.ToolUses = nil
	clone[0].AssistantResponseMessage.Content = "rewritten"
	clone[1].UserInputMessage.UserInputMessageContext.ToolResults = nil
	clone[1].UserInputMessage.UserInputMessageContext = nil

	if len(history[0].AssistantResponseMessage.ToolUses) != 1 {
		t.Fatalf("caller's tool uses were nil'd through the clone")
	}
	if history[0].AssistantResponseMessage.Content == "rewritten" {
		t.Fatalf("caller's assistant content was rewritten through the clone")
	}
	if history[1].UserInputMessage.UserInputMessageContext == nil {
		t.Fatalf("caller's user input context was nil'd through the clone")
	}
	if len(history[1].UserInputMessage.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("caller's tool results were nil'd through the clone")
	}
}
