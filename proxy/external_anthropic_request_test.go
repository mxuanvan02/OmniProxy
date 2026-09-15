package proxy

import (
	"encoding/json"
	"testing"

	"omniproxy/config"
)

// The Messages API is stricter than the OpenAI dialects about the shape of a
// conversation: system is a top-level field rather than a leading message,
// tool_result blocks must precede the text of the turn that carries them, and
// max_tokens is mandatory. Each rule below is a 400 the caller could not
// recover from, so each one is asserted rather than assumed.

type anthropicRequestBody struct {
	Model     string `json:"model"`
	System    string `json:"system"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			// Content is the tool_result payload, which the API accepts as either
			// a string or a block list; the adapter sends the joined string form.
			Content   interface{}            `json:"content"`
			ToolUseID string                 `json:"tool_use_id"`
			ID        string                 `json:"id"`
			Name      string                 `json:"name"`
			Input     map[string]interface{} `json:"input"`
			IsError   bool                   `json:"is_error"`
		} `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string                 `json:"name"`
		InputSchema map[string]interface{} `json:"input_schema"`
	} `json:"tools"`
	ToolChoice  map[string]interface{} `json:"tool_choice"`
	Thinking    map[string]interface{} `json:"thinking"`
	Temperature float64                `json:"temperature"`
}

func decodeAnthropicBody(t *testing.T, payload *KiroPayload, account *config.Account) anthropicRequestBody {
	t.Helper()
	body, err := kiroPayloadToAnthropicRequest(payload, account)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var decoded anthropicRequestBody
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return decoded
}

// anthropicTestPayload builds the payload the translators hand to an adapter:
// a priming pair at the head of history, one completed tool round, and a
// current user turn carrying the tool result.
func anthropicTestPayload() *KiroPayload {
	payload := &KiroPayload{}
	payload.OriginalModel = "claude-sonnet-5"
	payload.hasPriming = true
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "You are a careful agent."}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{UserInputMessage: &KiroUserInputMessage{Content: "read the file"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content:  "reading it now",
			ToolUses: []KiroToolUse{{ToolUseID: "toolu_1", Name: "read_file", Input: map[string]interface{}{"path": "a.txt"}}},
		}},
	}
	current := &payload.ConversationState.CurrentMessage.UserInputMessage
	current.Content = "here is what it said"
	current.UserInputMessageContext = &UserInputMessageContext{
		ToolResults: []KiroToolResult{{
			ToolUseID: "toolu_1",
			Content:   []KiroResultContent{{Text: "hello from the file"}},
			Status:    "success",
		}},
	}
	return payload
}

func TestAnthropicRequestLiftsSystemPriming(t *testing.T) {
	body := decodeAnthropicBody(t, anthropicTestPayload(), nil)

	if body.System != "You are a careful agent." {
		t.Fatalf("system = %q, want the priming prompt", body.System)
	}
	// The pair must be gone from the conversation, or the model reads the
	// instructions twice — once as a system field and once as a chat turn.
	for _, m := range body.Messages {
		for _, b := range m.Content {
			if b.Text == "You are a careful agent." || b.Text == "I will follow these instructions." {
				t.Fatalf("priming pair still present as a message: %+v", m)
			}
		}
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, user)", len(body.Messages))
	}
	if body.Messages[0].Content[0].Text != "read the file" {
		t.Fatalf("first turn = %+v, want the turn after the priming pair", body.Messages[0])
	}
}

// A turn that carries a tool result must open with it: the API rejects text
// that precedes the tool_result answering the previous assistant turn.
func TestAnthropicUserTurnPutsToolResultsFirst(t *testing.T) {
	body := decodeAnthropicBody(t, anthropicTestPayload(), nil)

	last := body.Messages[len(body.Messages)-1]
	if len(last.Content) != 2 {
		t.Fatalf("current turn has %d blocks, want tool_result then text", len(last.Content))
	}
	if last.Content[0].Type != "tool_result" || last.Content[1].Type != "text" {
		t.Fatalf("block order = %q, %q; want tool_result, text", last.Content[0].Type, last.Content[1].Type)
	}
	if last.Content[0].ToolUseID != "toolu_1" || last.Content[0].Content != "hello from the file" {
		t.Fatalf("tool_result = %+v", last.Content[0])
	}
	if last.Content[0].IsError {
		t.Fatalf("a successful result was flagged as an error")
	}
}

func TestAnthropicAssistantTurnCarriesToolUse(t *testing.T) {
	body := decodeAnthropicBody(t, anthropicTestPayload(), nil)

	assistant := body.Messages[1]
	if assistant.Role != "assistant" || assistant.Content[0].Type != "text" {
		t.Fatalf("assistant turn = %+v", assistant)
	}
	call := assistant.Content[1]
	if call.Type != "tool_use" || call.ID != "toolu_1" || call.Name != "read_file" {
		t.Fatalf("tool_use block = %+v", call)
	}
	if call.Input["path"] != "a.txt" {
		t.Fatalf("tool_use input = %+v, want the arguments preserved", call.Input)
	}
}

// max_tokens is the one field whose absence is a 400 rather than a default, so
// a payload carrying no inference config still has to name a ceiling.
func TestAnthropicMaxTokensIsAlwaysPresent(t *testing.T) {
	payload := anthropicTestPayload()
	if payload.InferenceConfig != nil {
		t.Fatalf("fixture should have no inference config for this case")
	}
	if got := decodeAnthropicBody(t, payload, nil).MaxTokens; got != externalAnthropicDefaultMaxTokens {
		t.Fatalf("max_tokens = %d, want the default %d", got, externalAnthropicDefaultMaxTokens)
	}

	payload.InferenceConfig = &InferenceConfig{MaxTokens: 1500}
	if got := decodeAnthropicBody(t, payload, nil).MaxTokens; got != 1500 {
		t.Fatalf("max_tokens = %d, want the requested 1500", got)
	}
}

// The translator is the only component that knows it injected the priming pair,
// so the adapter reads its flag rather than matching the assistant text. This
// checks the two agree on a payload built by ClaudeToKiro, which is the only
// way the adapter ever receives one.
func TestAnthropicRequestAcceptsTranslatorPriming(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-5",
		System: "You are a careful agent.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read the file"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]interface{}{"path": "a.txt"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_1", "content": "hello"},
				map[string]interface{}{"type": "text", "text": "what next"},
			}},
		},
	}
	payload := ClaudeToKiro(req, false)
	if !payload.hasPriming {
		t.Fatalf("ClaudeToKiro did not record the priming pair")
	}

	body := decodeAnthropicBody(t, payload, nil)
	if body.System != "You are a careful agent." {
		t.Fatalf("system = %q, want the request's system prompt", body.System)
	}
	last := body.Messages[len(body.Messages)-1]
	if len(last.Content) < 2 || last.Content[0].Type != "tool_result" {
		t.Fatalf("tool history was lost in the round trip: %+v", last.Content)
	}
}

// An assistant turn with no text and no tool call has no content the API will
// accept, and a Kiro history entry holds nothing else to put there.
func TestAnthropicEmptyAssistantTurnIsSkipped(t *testing.T) {
	payload := anthropicTestPayload()
	payload.ConversationState.History = append(payload.ConversationState.History,
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{}})
	payload.ConversationState.History = append(payload.ConversationState.History,
		KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: "still there?"}})

	body := decodeAnthropicBody(t, payload, nil)
	for _, m := range body.Messages {
		if m.Role == "assistant" && len(m.Content) == 0 {
			t.Fatalf("emitted an assistant turn with no content")
		}
	}
	roles := make([]string, 0, len(body.Messages))
	for _, m := range body.Messages {
		roles = append(roles, m.Role)
	}
	for i := 1; i < len(roles); i++ {
		if roles[i] == roles[i-1] {
			t.Fatalf("roles do not alternate: %v", roles)
		}
	}
}
