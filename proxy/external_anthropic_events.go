// Package proxy — one Anthropic Messages SSE event, decoded.
//
// Split from the read loop because the vocabulary is wide and purely
// mechanical: every case here is a shape rule to check against the API
// documentation, and keeping the two apart keeps each file readable.
package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicStreamState is the per-attempt state an event may read or update.
type anthropicStreamState struct {
	inputTokens  int
	outputTokens int
	blocks       map[int]*anthropicBlockAccum
	sawOutput    bool
	events       int
}

func newAnthropicStreamState() *anthropicStreamState {
	return &anthropicStreamState{blocks: map[int]*anthropicBlockAccum{}}
}

// emitText forwards a fragment and records that the turn produced output. The
// caller's OnOutput fires with it: retry safety depends on the attempt being
// marked as producing output at the moment it does.
func (s *anthropicStreamState) emitText(callback *KiroStreamCallback, text string, isThinking bool) {
	if text == "" {
		return
	}
	s.sawOutput = true
	if callback.OnOutput != nil {
		callback.OnOutput()
	}
	if callback.OnText != nil {
		callback.OnText(text, isThinking)
	}
}

// emitToolUse closes an accumulated tool_use block. The call is only complete
// once its block stops, which is why nothing is forwarded from the deltas.
func (s *anthropicStreamState) emitToolUse(callback *KiroStreamCallback, acc *anthropicBlockAccum) {
	if acc == nil || acc.name == "" {
		return
	}
	var input map[string]interface{}
	if strings.TrimSpace(acc.partialJSON) != "" {
		if err := json.Unmarshal([]byte(acc.partialJSON), &input); err != nil {
			// Fall back to a raw wrapper so the client still sees the args.
			input = map[string]interface{}{"_raw": acc.partialJSON}
		}
	}
	s.emitToolCall(callback, acc.id, acc.name, input)
}

// emitToolCall forwards a complete tool call, whether it was accumulated from
// deltas or arrived whole in a non-streaming body.
func (s *anthropicStreamState) emitToolCall(callback *KiroStreamCallback, id, name string, input map[string]interface{}) {
	if name == "" {
		return
	}
	s.sawOutput = true
	if callback.OnOutput != nil {
		callback.OnOutput()
	}
	if callback.OnToolUse == nil {
		return
	}
	// An argument-less tool call still reaches the client as an object, never
	// as a nil the caller would have to special-case.
	if input == nil {
		input = map[string]interface{}{}
	}
	callback.OnToolUse(KiroToolUse{ToolUseID: id, Name: name, Input: input})
}

// parseAnthropicSSEEvent handles one data payload and reports what it carried.
func parseAnthropicSSEEvent(data string, callback *KiroStreamCallback, state *anthropicStreamState) (externalSSELineResult, error) {
	var event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Message *struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return externalSSELineResult{}, fmt.Errorf("external anthropic SSE parse: %w", err)
	}

	// An explicit error object can arrive inside an HTTP 200 stream, the same
	// way it does on the OpenAI dialects, and must not be read as an empty but
	// successful turn.
	if event.Error != nil && event.Error.Message != "" {
		message := event.Error.Message
		if event.Error.Type != "" {
			message = event.Error.Type + ": " + message
		}
		// The observation flags decide whether the caller may replay the
		// attempt, so they describe this stream rather than the error.
		return externalSSELineResult{}, &externalSSEProviderError{
			message:            message,
			priorEventObserved: state.events > 1,
			outputObserved:     state.sawOutput,
		}
	}

	result := externalSSELineResult{recognized: true}
	switch event.Type {
	case "message_start":
		if event.Message != nil && event.Message.Usage != nil {
			usage := event.Message.Usage
			state.inputTokens = usage.InputTokens
			if usage.CacheReadInputTokens > 0 && callback.OnCacheRead != nil {
				callback.OnCacheRead(usage.CacheReadInputTokens)
			}
			if usage.CacheCreationInputTokens > 0 && callback.OnCacheCreate != nil {
				callback.OnCacheCreate(usage.CacheCreationInputTokens)
			}
		}
	case "content_block_start":
		if event.ContentBlock == nil {
			return result, nil
		}
		acc := state.blocks[event.Index]
		if acc == nil {
			acc = &anthropicBlockAccum{}
			state.blocks[event.Index] = acc
		}
		acc.kind = strings.ToLower(strings.TrimSpace(event.ContentBlock.Type))
		acc.id = event.ContentBlock.ID
		acc.name = event.ContentBlock.Name
		// Some gateways put the whole text in the start event rather than
		// streaming deltas. Emitting it costs nothing on the ones that do not,
		// because the field is empty in the delta form.
		if acc.kind == "text" && event.ContentBlock.Text != "" {
			state.emitText(callback, event.ContentBlock.Text, false)
			result.recognizedOutput = true
		}
	case "content_block_delta":
		if event.Delta == nil {
			return result, nil
		}
		switch strings.ToLower(strings.TrimSpace(event.Delta.Type)) {
		case "text_delta":
			state.emitText(callback, event.Delta.Text, false)
			result.recognizedOutput = event.Delta.Text != ""
		case "thinking_delta":
			state.emitText(callback, event.Delta.Thinking, true)
			result.recognizedOutput = event.Delta.Thinking != ""
		case "input_json_delta":
			if acc := state.blocks[event.Index]; acc != nil {
				acc.partialJSON += event.Delta.PartialJSON
			}
		}
	case "content_block_stop":
		if acc := state.blocks[event.Index]; acc != nil && acc.kind == "tool_use" {
			state.emitToolUse(callback, acc)
			result.recognizedOutput = true
		}
		delete(state.blocks, event.Index)
	case "message_delta":
		if event.Usage != nil {
			state.outputTokens = event.Usage.OutputTokens
		}
		if event.Delta != nil && event.Delta.StopReason != "" {
			result.stopReason = normalizeAnthropicStopReason(event.Delta.StopReason)
		}
	case "message_stop":
		result.terminal = true
	}
	return result, nil
}
