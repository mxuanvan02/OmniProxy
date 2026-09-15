// Package proxy — Anthropic Messages non-streaming parser for external
// accounts.
//
// A gateway may answer a request that asked for a stream with a complete
// message instead. The events emitted here are the same ones the SSE parser
// emits, so the two paths differ only in framing.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func parseExternalAnthropicJSON(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	gate := newBlankOutputGate(callback)
	callback = gate.callback()

	var message struct {
		Content []struct {
			Type     string                 `json:"type"`
			Text     string                 `json:"text"`
			Thinking string                 `json:"thinking"`
			ID       string                 `json:"id"`
			Name     string                 `json:"name"`
			Input    map[string]interface{} `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("external anthropic read: %w", err)
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return fmt.Errorf("external anthropic decode: %w", err)
	}
	// A gateway may answer HTTP 200 with an error envelope, in Anthropic's own
	// shape or in the OpenAI one. Both carry the message under "error", so one
	// check covers the risk that a reseller wraps an upstream refusal.
	if message.Error != nil && message.Error.Message != "" {
		text := message.Error.Message
		if message.Error.Type != "" {
			text = message.Error.Type + ": " + text
		}
		return &externalSSEProviderError{message: text, priorEventObserved: true}
	}

	state := newAnthropicStreamState()
	for _, block := range message.Content {
		switch strings.ToLower(strings.TrimSpace(block.Type)) {
		case "text":
			state.emitText(callback, block.Text, false)
		case "thinking":
			state.emitText(callback, block.Thinking, true)
		case "tool_use":
			state.emitToolCall(callback, block.ID, block.Name, block.Input)
		}
	}

	stopReason := normalizeAnthropicStopReason(message.StopReason)
	if !state.sawOutput {
		return fmt.Errorf("external anthropic response carried no assistant output")
	}
	if !gate.meaningful {
		return blankTurnError(stopReason)
	}
	if message.Usage != nil {
		if message.Usage.CacheReadInputTokens > 0 && callback.OnCacheRead != nil {
			callback.OnCacheRead(message.Usage.CacheReadInputTokens)
		}
		if message.Usage.CacheCreationInputTokens > 0 && callback.OnCacheCreate != nil {
			callback.OnCacheCreate(message.Usage.CacheCreationInputTokens)
		}
	}
	if callback.OnStopReason != nil {
		callback.OnStopReason(stopReason)
	}
	if callback.OnComplete != nil {
		inputTokens, outputTokens := 0, 0
		if message.Usage != nil {
			inputTokens = message.Usage.InputTokens
			outputTokens = message.Usage.OutputTokens
		}
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}
