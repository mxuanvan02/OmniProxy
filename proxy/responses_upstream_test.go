package proxy

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateResponsesGolden rewrites the golden file instead of comparing against it.
// Used once before the Phase 01 refactor, and any time the Codex request shape is
// intentionally changed.
var updateResponsesGolden = flag.Bool("update-responses-golden", false,
	"rewrite proxy/testdata/codex_responses_body.golden.json")

// TestCodexResponsesBodyGolden pins the exact request body the Codex dialect
// produces for a payload covering every branch: system priming pair, history with
// tool use, tool result, image, assistant text, and inference config.
//
// The body is produced by the shared builder through the Codex wrapper, so this
// test fails if moving the builder to responses_upstream.go changes a single byte
// of Codex behaviour.
func TestCodexResponsesBodyGolden(t *testing.T) {
	payload := goldenResponsesPayload()
	body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
	if err != nil {
		t.Fatalf("build codex responses body: %v", err)
	}
	got, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "codex_responses_body.golden.json")
	if *updateResponsesGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-responses-golden first): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("codex responses body changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// goldenResponsesPayload hand-builds the payload the golden was captured from.
//
// It is deliberately assembled with struct literals rather than by calling
// ClaudeToKiro or OpenAIToKiro: the golden must record the Codex response body
// shape only. Routing the payload through a client-side translator would make the
// golden churn whenever that translator changes, destroying its value as a
// regression gate.
//
// Every branch of kiroPayloadToCodexResponsesRequest is exercised:
//   - the priming pair (history[0] user / history[1] assistant "i will follow")
//     is lifted into top-level "instructions" and dropped from history
//   - a history user message with tool results → function_call_output items
//   - a history assistant message with text and tool uses → message +
//     function_call items
//   - the current message carries text plus an image → message with array content
//   - tools are rewritten by codexToolDescription, including the TaskStop guidance
//   - inference config contributes reasoning.effort but never temperature/top_p
//   - an explicit tool choice is translated to the flat Responses vocabulary
func goldenResponsesPayload() *KiroPayload {
	payload := &KiroPayload{
		ToolNameMap: map[string]string{
			"read":     "Read",
			"taskStop": "TaskStop",
		},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "read"},
	}
	payload.OriginalModel = "gpt-5.6-sol"
	payload.ConversationState.ConversationID = "conv-golden"
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.InferenceConfig = &InferenceConfig{
		MaxTokens:       4096,
		Temperature:     0.4,
		TopP:            0.9,
		ReasoningEffort: "medium",
	}

	// Priming pair: user system prompt followed by the assistant acknowledgement
	// that marks it as a system prompt rather than real conversation.
	systemUser := &KiroUserInputMessage{
		Content: "You are a coding agent. Follow the repository instructions.",
		Origin:  "AI_EDITOR",
	}
	primingAssistant := &KiroAssistantResponseMessage{
		Content: "I will follow the repository instructions.",
	}

	historyUser := &KiroUserInputMessage{
		Content: "inspect the repository",
		Origin:  "AI_EDITOR",
	}
	historyAssistant := &KiroAssistantResponseMessage{
		Content: "Let me read the entry point.",
		ToolUses: []KiroToolUse{{
			ToolUseID: "call_history_1",
			Name:      "read",
			Input:     map[string]interface{}{"path": "main.go"},
		}},
	}
	historyToolResult := &KiroUserInputMessage{
		Origin: "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: []KiroToolResult{{
				ToolUseID: "call_history_1",
				Content:   []KiroResultContent{{Text: "package main"}},
				Status:    "success",
			}},
		},
	}

	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: systemUser},
		{AssistantResponseMessage: primingAssistant},
		{UserInputMessage: historyUser},
		{AssistantResponseMessage: historyAssistant},
		{UserInputMessage: historyToolResult},
	}

	// Current message: text, an image, tool results for this turn, and the tool
	// catalog the client advertised.
	ctx := &UserInputMessageContext{
		ToolResults: []KiroToolResult{{
			ToolUseID: "call_current_1",
			Content:   []KiroResultContent{{Text: "file contents"}},
			Status:    "success",
		}},
	}
	readTool := KiroToolWrapper{}
	readTool.ToolSpecification.Name = "read"
	readTool.ToolSpecification.Description = "Read a file from disk."
	readTool.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
	}}
	ctx.Tools = append(ctx.Tools, readTool)

	stopTool := KiroToolWrapper{}
	stopTool.ToolSpecification.Name = "taskStop"
	stopTool.ToolSpecification.Description = "Stop a background task."
	stopTool.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{"type": "object"}}
	ctx.Tools = append(ctx.Tools, stopTool)

	image := KiroImage{Format: "png"}
	image.Source.Bytes = "iVBORw0KGgoAAAANSUhEUg=="

	cur := &payload.ConversationState.CurrentMessage.UserInputMessage
	cur.Content = "Summarize the entry point in the attached screenshot."
	cur.ModelID = "gpt-5.6-sol"
	cur.Origin = "AI_EDITOR"
	cur.Images = []KiroImage{image}
	cur.UserInputMessageContext = ctx

	return payload
}
