package proxy

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateResponsesGolden rewrites the golden files instead of comparing against
// them. Used once before the Phase 01 refactor, and any time the Codex request
// shape is intentionally changed.
var updateResponsesGolden = flag.Bool("update-responses-golden", false,
	"rewrite the proxy/testdata/codex_responses_*.golden.json files")

// responsesGoldenCase pairs a hand-built payload with the golden file that pins
// the exact body the Codex dialect emits for it.
type responsesGoldenCase struct {
	name    string
	golden  string
	payload func() *KiroPayload
}

// TestCodexResponsesBodyGolden pins the exact request body the Codex dialect
// produces, in two shapes:
//
//   - "full" — system priming pair, history with tool use, tool result, image,
//     assistant text, tool catalog (including an empty-description TaskStop
//     alias) and inference config.
//   - "bare-system-prompt" — no priming pair; history[0] is a lone user message
//     opening with "You are ", which the builder lifts into "instructions"
//     through its second, otherwise-unreachable instructions source.
//
// The body is produced by the shared builder through the Codex wrapper, so this
// test fails if moving the builder to responses_upstream.go changes a single
// byte of Codex behaviour.
//
// Both payloads deliberately leave OriginalModel and
// CurrentMessage.UserInputMessage.ModelID empty, so the builder's hardcoded
// default model is on the only path that can supply "model" — mutating that
// literal fails these tests. Setting either field would satisfy the precedence
// chain earlier and silently make the default dead code.
func TestCodexResponsesBodyGolden(t *testing.T) {
	cases := []responsesGoldenCase{
		{
			name:    "full",
			golden:  "codex_responses_body.golden.json",
			payload: goldenResponsesPayload,
		},
		{
			name:    "bare-system-prompt",
			golden:  "codex_responses_bare_system_prompt.golden.json",
			payload: goldenBareSystemPromptResponsesPayload,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.payload()

			// Guard the gate itself: if a future edit re-populates either model
			// source the golden would keep passing while no longer pinning the
			// builder's default model.
			if payload.OriginalModel != "" {
				t.Fatalf("fixture sets OriginalModel=%q; the builder's default model would no longer be gated", payload.OriginalModel)
			}
			if id := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID; id != "" {
				t.Fatalf("fixture sets CurrentMessage.UserInputMessage.ModelID=%q; the builder's default model would no longer be gated", id)
			}

			body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
			if err != nil {
				t.Fatalf("build codex responses body: %v", err)
			}
			got, err := json.MarshalIndent(body, "", "  ")
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			got = append(got, '\n')

			path := filepath.Join("testdata", tc.golden)
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
		})
	}
}

// TestCodexResponsesModelPrecedence pins the builder's three-level model
// resolution and, crucially, the ORDER of the first two levels:
//
//  1. payload.OriginalModel (the client's requested model)
//  2. payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
//  3. the hardcoded default "gpt-5.6-sol"
//
// Both golden payloads leave levels 1 and 2 empty on purpose, so they pin level
// 3 and nothing else — without this test a dropped or reordered level above the
// default would be silent, and every request would quietly resolve to a
// lower-precedence model. Phase 01 relocates exactly this chain into
// proxy/responses_upstream.go, which is what makes the pin load-bearing.
//
// Phase 01 also replaces level 3 with a configurable
// responsesDialectOptions.DefaultModel and adds an error branch for the
// no-model case; whoever makes that change must extend this test.
func TestCodexResponsesModelPrecedence(t *testing.T) {
	// Three distinct literals so a mix-up between any two levels cannot pass.
	const (
		defaultModel  = "gpt-5.6-sol"
		originalModel = "gpt-5.6-terra"
		messageModel  = "gpt-5.6-luna"
	)

	cases := []struct {
		name          string
		originalModel string
		messageModel  string
		want          string
	}{
		// Level 1 fires on its own.
		{"original-only", originalModel, "", originalModel},
		// Level 1 beats a *different* level 2 — this is what pins the order of
		// the two levels; swapping them still passes the other level-1 case.
		{"original-beats-message", originalModel, messageModel, originalModel},
		// Level 2 is reachable only once level 1 is empty.
		{"message-only", "", messageModel, messageModel},
		// Level 3 is the last resort, with both sources empty.
		{"default", "", "", defaultModel},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := &KiroPayload{OriginalModel: tc.originalModel}
			payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = tc.messageModel

			body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
			if err != nil {
				t.Fatalf("build codex responses body: %v", err)
			}
			if got := body["model"]; got != tc.want {
				t.Fatalf("model = %v, want %q (OriginalModel=%q ModelID=%q)",
					got, tc.want, tc.originalModel, tc.messageModel)
			}
		})
	}
}

// goldenResponsesPayload hand-builds the payload the "full" golden was captured
// from.
//
// It is deliberately assembled with struct literals rather than by calling
// ClaudeToKiro or OpenAIToKiro: the golden must record the Codex response body
// shape only. Routing the payload through a client-side translator would make the
// golden churn whenever that translator changes, destroying its value as a
// regression gate.
//
// Branches of the builder this payload exercises:
//   - the priming pair (history[0] user / history[1] assistant "i will follow")
//     is lifted into top-level "instructions" and dropped from history
//   - a history user message with tool results → function_call_output items
//   - a history assistant message with text and tool uses → message +
//     function_call items
//   - the current message carries text plus an image → message with array content
//   - tools are rewritten by codexToolDescription: the plain pass-through, the
//     described-TaskStop arm, and (via the "taskStopBare" alias) the
//     empty-description arm
//   - inference config contributes reasoning.effort but never temperature/top_p
//   - an explicit tool choice is translated to the flat Responses vocabulary
//   - a nil account skips model remapping
//
// Branches this payload does NOT reach (they are not pinned here):
//   - the nil-payload early error return
//   - a non-nil account, and any model carrying an internal prefix
//   - a history user message that is image-only (no text)
//   - a tool result with an empty Content slice
//   - InferenceConfig non-nil but with an empty ReasoningEffort
//   - instructions == "" and no tools, i.e. a body with neither key — the
//     "bare-system-prompt" case covers the no-tools half of that
//   - codexMessageContent with images but empty text
func goldenResponsesPayload() *KiroPayload {
	payload := &KiroPayload{
		ToolNameMap: map[string]string{
			"read":         "Read",
			"taskStop":     "TaskStop",
			"taskStopBare": "TaskStop",
		},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "read"},
	}
	// NOTE: OriginalModel and CurrentMessage.UserInputMessage.ModelID are
	// deliberately left EMPTY. Setting either would satisfy the model
	// precedence chain before it reaches the builder's hardcoded default,
	// making that default dead code and leaving the golden blind to it.
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

	// A second alias that maps back to TaskStop but advertises no description,
	// driving codexToolDescription's empty-description arm (guidance returned
	// verbatim, with no leading blank line).
	stopBareTool := KiroToolWrapper{}
	stopBareTool.ToolSpecification.Name = "taskStopBare"
	stopBareTool.ToolSpecification.Description = ""
	stopBareTool.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{"type": "object"}}
	ctx.Tools = append(ctx.Tools, stopBareTool)

	image := KiroImage{Format: "png"}
	image.Source.Bytes = "iVBORw0KGgoAAAANSUhEUg=="

	cur := &payload.ConversationState.CurrentMessage.UserInputMessage
	cur.Content = "Summarize the entry point in the attached screenshot."
	cur.Origin = "AI_EDITOR"
	cur.Images = []KiroImage{image}
	cur.UserInputMessageContext = ctx

	return payload
}

// goldenBareSystemPromptResponsesPayload hand-builds the payload behind the
// "bare-system-prompt" golden: no priming pair, so the builder's second
// instructions source (a lone leading user message opening with "You are ")
// is the one that fires.
//
// That arm is dead in goldenResponsesPayload because the priming pair wins
// first, and the Phase 01 refactor relocates it to proxy/responses_upstream.go
// — a wrong history index during the move would otherwise go unnoticed.
//
// Like the full fixture it leaves OriginalModel and
// CurrentMessage.UserInputMessage.ModelID empty, so this case gates the
// builder's default model too. It carries no tools, no images, no tool results
// and no inference config, which additionally pins the "omit these keys when
// absent" behaviour.
func goldenBareSystemPromptResponsesPayload() *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.ConversationID = "conv-bare-system-prompt"
	payload.ConversationState.ChatTriggerType = "MANUAL"

	// Opens with "You are " so the fallback recognises it. The assistant turn
	// that follows deliberately does NOT contain "i will follow", otherwise the
	// priming-pair branch would claim it instead.
	systemUser := &KiroUserInputMessage{
		Content: "You are a bare system prompt injected by a non-Claude client.",
		Origin:  "AI_EDITOR",
	}
	systemAck := &KiroAssistantResponseMessage{
		Content: "Understood, starting now.",
	}
	historyUser := &KiroUserInputMessage{
		Content: "inspect the repository",
		Origin:  "AI_EDITOR",
	}

	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: systemUser},
		{AssistantResponseMessage: systemAck},
		{UserInputMessage: historyUser},
	}

	cur := &payload.ConversationState.CurrentMessage.UserInputMessage
	cur.Content = "Summarize the plan."
	cur.Origin = "AI_EDITOR"

	return payload
}
