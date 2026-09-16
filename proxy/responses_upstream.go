// Package proxy — shared translation between the KiroPayload intermediate
// representation and the OpenAI Responses API, in both directions.
//
// The Responses wire shape is the same whether the upstream is OpenAI's own
// /v1/responses, a ChatGPT Codex subscription backend, or a resale gateway that
// speaks Responses. What differs is a small set of behaviours, carried here as
// responsesDialectOptions so callers share one implementation instead of
// drifting copies.
package proxy

import (
	"encoding/json"
	"fmt"
	"omniproxy/config"
	"strings"
)

// responsesDialectOptions carries the differences between upstreams that speak
// the Responses API. Everything else about building a request and parsing a
// response is shared.
type responsesDialectOptions struct {
	// DefaultModel is used when the payload carries no model ID. Empty means a
	// payload without a model is an error rather than a silent substitution —
	// a generic gateway has no sensible default to guess.
	DefaultModel string

	// ForwardSamplingParams sends temperature and top_p. The ChatGPT Codex
	// backend rejects both with HTTP 400 ("Unsupported parameter: temperature")
	// on GPT-5.x reasoning models, so Codex leaves this false. Generic
	// OpenAI-compatible gateways normally accept them.
	ForwardSamplingParams bool

	// ToolDescription rewrites a tool description before it is sent. Codex uses
	// it to inject CLI lifecycle guidance; nil passes the description through.
	ToolDescription func(name, description string) string
}

// toolDescription applies the dialect's description rewriting, if any.
func (o responsesDialectOptions) toolDescription(name, description string) string {
	if o.ToolDescription == nil {
		return description
	}
	return o.ToolDescription(name, description)
}

// kiroPayloadToResponsesRequest converts the KiroPayload into a
// /v1/responses request body. The Responses API uses an "input" array of
// typed items (message / function_call / function_call_output) rather than
// chat-completions "messages".
func kiroPayloadToResponsesRequest(payload *KiroPayload, account *config.Account, opts responsesDialectOptions) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("nil payload")
	}

	modelID := strings.TrimSpace(payload.OriginalModel)
	if modelID == "" {
		modelID = strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	}
	if modelID == "" {
		modelID = opts.DefaultModel
	}
	if modelID == "" {
		return nil, fmt.Errorf("responses request: payload carries no model id")
	}
	modelID = stripInternalModelPrefix(modelID)
	if account != nil {
		modelID = resolveExternalModelID(account, modelID)
	}

	input := make([]map[string]interface{}, 0, 8)
	history := payload.ConversationState.History

	// Detect & extract the system priming pair injected by the translators
	// (history[0]=user(systemPrompt), history[1]=assistant("I will follow...")).
	// Responses API has a top-level "instructions" field for system priming.
	instructions := ""
	if len(history) >= 2 {
		first := history[0]
		second := history[1]
		if first.UserInputMessage != nil && second.AssistantResponseMessage != nil &&
			strings.Contains(strings.ToLower(strings.TrimSpace(second.AssistantResponseMessage.Content)), "i will follow") {
			instructions = strings.TrimSpace(first.UserInputMessage.Content)
			history = history[2:]
		}
	}

	// If no priming pair was detected, look for a leading user-only system
	// prompt (some translators inject it that way for non-Claude clients).
	if instructions == "" && len(history) > 0 {
		if history[0].UserInputMessage != nil && strings.HasPrefix(strings.TrimSpace(history[0].UserInputMessage.Content), "You are ") {
			instructions = strings.TrimSpace(history[0].UserInputMessage.Content)
			history = history[1:]
		}
	}

	for _, h := range history {
		if h.UserInputMessage != nil {
			um := h.UserInputMessage
			// Tool results → function_call_output items.
			if um.UserInputMessageContext != nil && len(um.UserInputMessageContext.ToolResults) > 0 {
				for _, tr := range um.UserInputMessageContext.ToolResults {
					text := ""
					if len(tr.Content) > 0 {
						text = tr.Content[0].Text
					}
					input = append(input, map[string]interface{}{
						"type":    "function_call_output",
						"call_id": tr.ToolUseID,
						"output":  text,
					})
				}
			}
			if strings.TrimSpace(um.Content) != "" || len(um.Images) > 0 {
				input = append(input, map[string]interface{}{
					"type":    "message",
					"role":    "user",
					"content": responsesMessageContent(um.Content, um.Images),
				})
			}
		} else if h.AssistantResponseMessage != nil {
			am := h.AssistantResponseMessage
			// Assistant text → message item. Tool uses → function_call items
			// (separate from the message; Responses API models them as
			// parallel output items in the same turn).
			if strings.TrimSpace(am.Content) != "" {
				input = append(input, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": am.Content,
				})
			}
			for _, tu := range am.ToolUses {
				args, _ := json.Marshal(tu.Input)
				input = append(input, map[string]interface{}{
					"type":      "function_call",
					"call_id":   tu.ToolUseID,
					"name":      restoreToolName(payload, tu.Name),
					"arguments": string(args),
				})
			}
		}
	}

	// Current user message + tool results.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		for _, tr := range cur.UserInputMessageContext.ToolResults {
			text := ""
			if len(tr.Content) > 0 {
				text = tr.Content[0].Text
			}
			input = append(input, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": tr.ToolUseID,
				"output":  text,
			})
		}
	}
	if strings.TrimSpace(cur.Content) != "" || len(cur.Images) > 0 {
		input = append(input, map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": responsesMessageContent(cur.Content, cur.Images),
		})
	}

	body := map[string]interface{}{
		"model":  modelID,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}

	// Tools — Responses API uses a flat shape (name/description/parameters
	// at top level, not nested under "function").
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(cur.UserInputMessageContext.Tools))
		for _, tw := range cur.UserInputMessageContext.Tools {
			name := restoreToolName(payload, tw.ToolSpecification.Name)
			tools = append(tools, map[string]interface{}{
				"type":        "function",
				"name":        name,
				"description": opts.toolDescription(name, tw.ToolSpecification.Description),
				"parameters":  tw.ToolSpecification.InputSchema.JSON,
			})
		}
		body["tools"] = tools
	}
	if choice := responsesToolChoice(payload.ToolChoice, payload); choice != nil {
		body["tool_choice"] = choice
	}

	if payload.InferenceConfig != nil {
		if payload.InferenceConfig.ReasoningEffort != "" {
			body["reasoning"] = map[string]string{"effort": payload.InferenceConfig.ReasoningEffort}
		}
		// max_output_tokens is a safety ceiling, not a sampling preference —
		// always forward it when the client sets one, regardless of whether
		// the upstream accepts temperature/top_p. Without this the upstream
		// falls back to its own default which may be lower than expected.
		if payload.InferenceConfig.MaxTokens > 0 {
			body["max_output_tokens"] = payload.InferenceConfig.MaxTokens
		}
		// Sampling parameters are opt-in per dialect: the ChatGPT Codex backend
		// rejects temperature/top_p with HTTP 400 for GPT-5.x reasoning models,
		// while a generic OpenAI-compatible gateway accepts them.
		if opts.ForwardSamplingParams {
			if payload.InferenceConfig.HasTemperature {
				body["temperature"] = payload.InferenceConfig.Temperature
			}
			if payload.InferenceConfig.TopP > 0 {
				body["top_p"] = payload.InferenceConfig.TopP
			}
		}
	}

	return body, nil
}

// responsesToolChoice converts Anthropic and Chat Completions tool-selection
// shapes to the flat vocabulary accepted by the Responses API.
func responsesToolChoice(choice interface{}, payload *KiroPayload) interface{} {
	switch value := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "any", "required":
			return "required"
		case "none", "auto":
			return strings.ToLower(strings.TrimSpace(value))
		default:
			return value
		}
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "any", "required":
			return "required"
		case "none", "auto":
			return strings.ToLower(strings.TrimSpace(typeName))
		case "tool", "function":
			name, _ := value["name"].(string)
			if name == "" {
				if fn, ok := value["function"].(map[string]interface{}); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name == "" {
				return nil
			}
			return map[string]interface{}{
				"type": "function",
				"name": restoreToolName(payload, name),
			}
		}
	}
	return choice
}

// responsesMessageContent builds the Responses API "content" value for a
// message item. Plain text → string; with images → array of
// {type:"input_text"|"input_image"} parts.
func responsesMessageContent(text string, images []KiroImage) interface{} {
	if len(images) == 0 {
		return text
	}
	parts := make([]map[string]interface{}, 0, len(images)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]interface{}{
			"type": "input_text",
			"text": text,
		})
	}
	for _, image := range images {
		format := strings.TrimSpace(image.Format)
		if format == "" {
			format = "png"
		}
		parts = append(parts, map[string]interface{}{
			"type":      "input_image",
			"image_url": "data:image/" + format + ";base64," + image.Source.Bytes,
		})
	}
	return parts
}
