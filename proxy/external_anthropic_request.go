// Package proxy — Anthropic Messages request translation for external accounts.
//
// The translation is the inverse of ClaudeToKiro: a KiroPayload, which flattens
// an Anthropic conversation into Kiro's history/currentMessage shape, is turned
// back into the shape an Anthropic-compatible gateway expects. Purely
// structural, no I/O, because the shape rules below are what the API rejects a
// request for and they need to be testable without a server.
package proxy

import (
	"fmt"
	"strings"

	"omniproxy/config"
)

// kiroPayloadToAnthropicRequest builds a POST /v1/messages body.
func kiroPayloadToAnthropicRequest(payload *KiroPayload, account *config.Account) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("nil payload")
	}

	body := map[string]interface{}{
		"model": externalWireModelID(payload, account),
	}

	history := payload.ConversationState.History
	// The translators encode the system prompt as a two-entry priming pair at
	// the head of history — user(systemPrompt) then assistant("I will follow…")
	// — because Kiro has no system field. The Messages API does, so the pair is
	// lifted back out. Detection reads payload.hasPriming rather than matching
	// the assistant text the way the chat builder does: the translator is the
	// only component that knows whether it injected the pair, and it records
	// that fact on the payload.
	if payload.hasPriming && len(history) >= 2 && history[0].UserInputMessage != nil {
		if systemPrompt := strings.TrimSpace(history[0].UserInputMessage.Content); systemPrompt != "" {
			body["system"] = systemPrompt
		}
		history = history[2:]
	}

	messages := make([]map[string]interface{}, 0, len(history)+1)
	for _, h := range history {
		switch {
		case h.UserInputMessage != nil:
			messages = appendAnthropicMessage(messages, "user", anthropicUserBlocks(h.UserInputMessage))
		case h.AssistantResponseMessage != nil:
			blocks := anthropicAssistantBlocks(h.AssistantResponseMessage, payload)
			if len(blocks) == 0 {
				// The API rejects an assistant turn with no content, and a Kiro
				// history entry holds nothing else that could go there.
				continue
			}
			messages = appendAnthropicMessage(messages, "assistant", blocks)
		}
	}

	current := payload.ConversationState.CurrentMessage.UserInputMessage
	messages = appendAnthropicMessage(messages, "user", anthropicUserBlocks(&current))
	// The conversation must open on a user turn. A history that begins with an
	// assistant message reaches here only from a client that supplied one.
	for len(messages) > 0 && messages[0]["role"] == "assistant" {
		messages = messages[1:]
	}
	body["messages"] = messages

	// Tools are attached to the current user turn by the translators, so they
	// are read from there rather than from history.
	if ctx := current.UserInputMessageContext; ctx != nil && len(ctx.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(ctx.Tools))
		for _, tw := range ctx.Tools {
			tools = append(tools, map[string]interface{}{
				"name":         restoreToolName(payload, tw.ToolSpecification.Name),
				"description":  tw.ToolSpecification.Description,
				"input_schema": anthropicToolSchema(tw.ToolSpecification.InputSchema.JSON),
			})
		}
		body["tools"] = tools
	}
	if choice := anthropicToolChoice(payload.ToolChoice, payload); choice != nil {
		body["tool_choice"] = choice
	}

	applyAnthropicSamplingParams(body, payload)
	return body, nil
}

// appendAnthropicMessage adds a turn, merging it into the previous one when the
// role repeats. The Messages API requires strictly alternating roles and
// rejects the request otherwise, while the Kiro history it is built from
// guarantees alternation only for well-formed input. Merging keeps such a
// request valid instead of failing it at the gateway.
func appendAnthropicMessage(messages []map[string]interface{}, role string, blocks []map[string]interface{}) []map[string]interface{} {
	if len(blocks) == 0 {
		return messages
	}
	if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
		previous, _ := messages[len(messages)-1]["content"].([]map[string]interface{})
		messages[len(messages)-1]["content"] = append(previous, blocks...)
		return messages
	}
	return append(messages, map[string]interface{}{"role": role, "content": blocks})
}

// anthropicUserBlocks renders one user turn. Tool results come first: the API
// rejects a turn whose text precedes the tool_result blocks that answer the
// previous assistant turn. The chat builder has no such rule — it emits
// standalone "tool" messages — so its ordering is deliberately not copied.
func anthropicUserBlocks(msg *KiroUserInputMessage) []map[string]interface{} {
	if msg == nil {
		return nil
	}
	blocks := make([]map[string]interface{}, 0, len(msg.Images)+2)
	if ctx := msg.UserInputMessageContext; ctx != nil {
		for _, tr := range ctx.ToolResults {
			block := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": tr.ToolUseID,
				"content":     kiroToolResultText(tr),
			}
			// Kiro marks a failed tool call on the result itself; the Messages
			// API carries the same fact as a flag on the block.
			if status := strings.ToLower(strings.TrimSpace(tr.Status)); status != "" && status != "success" {
				block["is_error"] = true
			}
			blocks = append(blocks, block)
		}
	}
	// A text block must be non-empty, so an empty content string is dropped
	// rather than sent as an empty block.
	if msg.Content != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": msg.Content})
	}
	for _, img := range msg.Images {
		if block := anthropicImageBlock(img); block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func anthropicAssistantBlocks(msg *KiroAssistantResponseMessage, payload *KiroPayload) []map[string]interface{} {
	if msg == nil {
		return nil
	}
	blocks := make([]map[string]interface{}, 0, len(msg.ToolUses)+1)
	if msg.Content != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": msg.Content})
	}
	for _, tu := range msg.ToolUses {
		input := tu.Input
		if input == nil {
			// An input-less tool call is sent as {} rather than omitted: the
			// API requires the field, and a client that reads it back expects
			// an object even when the tool took no arguments.
			input = map[string]interface{}{}
		}
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tu.ToolUseID,
			"name":  restoreToolName(payload, tu.Name),
			"input": input,
		})
	}
	return blocks
}

// kiroToolResultText joins every content part. The chat builder reads only the
// first part because the OpenAI tool message takes a single string; joining
// loses nothing here and survives a multi-part result.
func kiroToolResultText(tr KiroToolResult) string {
	if len(tr.Content) == 0 {
		return ""
	}
	if len(tr.Content) == 1 {
		return tr.Content[0].Text
	}
	var b strings.Builder
	for i, part := range tr.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part.Text)
	}
	return b.String()
}
