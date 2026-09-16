// Package proxy — Anthropic Messages parameters and content blocks.
//
// These are the pieces of a Messages request that need a rule rather than a
// copy: the API is stricter than the OpenAI dialects about several of them, and
// each rejection is a 400 the caller cannot recover from.
package proxy

import (
	"strings"
)

// The Messages API requires max_tokens on every request and rejects one that
// omits it, so a payload carrying no inference config still has to name a
// ceiling. 8192 matches the ceiling used elsewhere in the proxy as a default.
const externalAnthropicDefaultMaxTokens = 8192

// Thinking is opt-in and its budget must stay below max_tokens. A request whose
// ceiling is at or below this budget is therefore sent without a thinking block
// rather than with one the API would reject.
const externalAnthropicThinkingBudget = 4096

// anthropicToolSchema returns a JSON Schema the API will accept as an
// input_schema. Reuses the external schema sanitiser — a malformed enum breaks
// every OpenAI-compatible provider for the same reason — and adds the root
// "type": "object" the Messages API requires and the chat dialect does not.
func anthropicToolSchema(schema interface{}) interface{} {
	cleaned := sanitizeExternalToolSchema(schema)
	if object, ok := cleaned.(map[string]interface{}); ok {
		if _, present := object["type"]; !present {
			object["type"] = "object"
		}
		return object
	}
	return map[string]interface{}{"type": "object"}
}

// anthropicToolChoice normalises the client's choice into the Messages API
// vocabulary. KiroPayload already carries Anthropic's own words — the Claude
// translator copies them through — so this maps the few spellings the API does
// not accept, and drops "none", which has no equivalent: leaving the field out
// is the API's own auto behaviour.
func anthropicToolChoice(choice interface{}, payload *KiroPayload) interface{} {
	switch value := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "any", "required":
			return map[string]interface{}{"type": "any"}
		case "auto":
			return map[string]interface{}{"type": "auto"}
		default:
			return nil
		}
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "any", "required":
			return map[string]interface{}{"type": "any"}
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "tool", "function":
			name, _ := value["name"].(string)
			if name == "" {
				if fn, ok := value["function"].(map[string]interface{}); ok {
					name, _ = fn["name"].(string)
				}
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return nil
			}
			// The name must match the tools list sent alongside it, which
			// carries restored rather than sanitized names.
			return map[string]interface{}{"type": "tool", "name": restoreToolName(payload, name)}
		}
	}
	return nil
}

// applyAnthropicSamplingParams writes the ceiling and the sampling fields onto
// a request body.
//
// The caller sets body["tool_choice"] first, so the thinking/forced-tool
// conflict below is resolved here rather than in the builder: the two fields
// that cannot coexist are written by two functions, and one of them has to look
// at what the other decided.
func applyAnthropicSamplingParams(body map[string]interface{}, payload *KiroPayload) {
	cfg := payload.InferenceConfig

	// max_tokens is required, so it is filled in even when the payload carries
	// no inference config at all — the one field whose absence is a 400 rather
	// than a default.
	maxTokens := externalAnthropicMaxTokens(cfg)
	body["max_tokens"] = maxTokens

	if thinking := anthropicThinking(cfg, maxTokens); thinking != nil {
		if anthropicToolChoiceForced(body["tool_choice"]) {
			// Manual extended thinking rejects a tool_choice that forces a call.
			// The forced call wins: dropping it would let the model answer
			// without the tool the client required, silently breaking its loop,
			// while dropping thinking only forgoes reasoning depth.
		} else {
			body["thinking"] = thinking
			// temperature and top_p are incompatible with manual thinking — the
			// API rejects the request rather than ignoring them — so the ceiling
			// is the only sampling field sent on this path.
			return
		}
	}

	if cfg == nil {
		return
	}
	// Anthropic accepts temperature and top_p in [0,1] and rejects the request
	// rather than clamping, while the OpenAI dialects accept up to 2. A client
	// asking for more gets the API's default here instead of a 400.
	if cfg.Temperature > 0 && cfg.Temperature <= 1 {
		body["temperature"] = cfg.Temperature
	}
	if cfg.TopP > 0 && cfg.TopP <= 1 {
		body["top_p"] = cfg.TopP
	}
}

// externalAnthropicMaxTokens reports the output ceiling to send.
func externalAnthropicMaxTokens(cfg *InferenceConfig) int {
	if cfg != nil && cfg.MaxTokens > 0 {
		return cfg.MaxTokens
	}
	return externalAnthropicDefaultMaxTokens
}

// anthropicToolChoiceForced reports whether a normalized tool_choice names or
// demands a call, as opposed to leaving the decision to the model.
func anthropicToolChoiceForced(choice interface{}) bool {
	shaped, ok := choice.(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := shaped["type"].(string)
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "any", "tool":
		return true
	default:
		return false
	}
}

// anthropicThinking enables extended thinking only when the client asked for it
// by naming a reasoning effort. A gateway that does not support thinking
// answers a request carrying it with a 400, so the field is never sent
// speculatively.
func anthropicThinking(cfg *InferenceConfig, maxTokens int) map[string]interface{} {
	if cfg == nil || strings.TrimSpace(cfg.ReasoningEffort) == "" {
		return nil
	}
	if maxTokens <= externalAnthropicThinkingBudget {
		return nil
	}
	return map[string]interface{}{
		"type":          "enabled",
		"budget_tokens": externalAnthropicThinkingBudget,
	}
}

func anthropicImageBlock(img KiroImage) map[string]interface{} {
	data := strings.TrimSpace(img.Source.Bytes)
	if data == "" {
		return nil
	}
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": anthropicMediaType(img.Format),
			"data":       data,
		},
	}
}

// anthropicMediaType maps the format Kiro carries back to a media type. Kiro
// stores the bare subtype, and an unrecognised one falls back to PNG, the
// format the API treats as the default.
func anthropicMediaType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
