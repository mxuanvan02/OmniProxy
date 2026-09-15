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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
		// Sampling parameters are opt-in per dialect: the ChatGPT Codex backend
		// rejects temperature/top_p with HTTP 400 for GPT-5.x reasoning models,
		// while a generic OpenAI-compatible gateway accepts them.
		if opts.ForwardSamplingParams {
			if payload.InferenceConfig.Temperature > 0 {
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

// bufferedReadCloser preserves bytes already buffered by bufio.Reader
// while retaining the upstream body's Close method for the SSE idle watchdog.
type bufferedReadCloser struct {
	io.Reader
	io.Closer
}

// parseResponsesSSE parses the Responses API SSE stream and
// drives the KiroStreamCallback. Output text/reasoning deltas are
// accumulated and emitted to callback.OnText; tool calls are accumulated
// per call_id and emitted via callback.OnToolUse on completion.
//
// Per-token overhead is bounded by reading line-by-line with bufio.Reader
// (one JSON parse per SSE event — same as OpenAI chat-completions path).
// Downstream coalescing happens at the handler side, not here.
func parseResponsesSSE(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	br := bufio.NewReader(body)

	// SSE idle watchdog: kills the connection when no ``data:`` line arrives
	// within the configured idle window. Catches "200 OK but silent" hangs
	// that a byte-level idle reader cannot detect (upstream keepalive
	// comments reset byte-level timers without carrying payload). The
	// watchdog closes the underlying body to unblock the pending ReadString.
	var watchdog *sseIdleWatchdog
	if rc, ok := body.(io.ReadCloser); ok {
		watchdog = newSSEIdleWatchdog(rc)
		if watchdog != nil {
			watchdog.Start()
			defer watchdog.Stop()
		}
	}

	var inputTokens, outputTokens int
	var totalCredits float64
	// toolAccums accumulates arguments per call_id; emitted on
	// response.output_item.done with type=function_call.
	toolAccums := make(map[string]*responsesToolAccum)
	completed := false

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if watchdog != nil && watchdog.TimedOut() {
				return ErrStreamIdleTimeout
			}
			if err == io.EOF {
				if strings.TrimSpace(line) != "" {
					result := processResponsesSSELine(line, callback, toolAccums, &inputTokens, &outputTokens)
					if result.err != nil {
						return result.err
					}
					completed = completed || result.completed
				}
				break
			}
			return fmt.Errorf("codex SSE read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// event: / id: / comment lines — ignore (event type is
			// duplicated inside the JSON payload's "type" field).
			continue
		}
		// Real data line — reset the SSE idle watchdog timer.
		if watchdog != nil {
			watchdog.DataReceived()
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		result := processResponsesSSELine("data: "+data, callback, toolAccums, &inputTokens, &outputTokens)
		if result.err != nil {
			return result.err
		}
		completed = completed || result.completed
	}

	// The Responses API's completion event is the acknowledgement that the
	// upstream accepted and completed the turn. A bare EOF or [DONE] before it
	// is a truncated stream, not an empty successful response. Treating that as
	// success made downstream clients receive a blank assistant turn.
	if !completed {
		return fmt.Errorf("SSE stream ended before response.completed")
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}
	return nil
}

// responsesToolAccum buffers function_call arguments across multiple
// response.function_call_arguments.delta events until the final
// response.output_item.done arrives.
type responsesToolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}

type responsesSSELineResult struct {
	completed bool
	err       error
}

// processResponsesSSELine parses one Responses API SSE data line and dispatches
// to the callback. Kept separate from parseResponsesSSE for testability.
func processResponsesSSELine(line string, callback *KiroStreamCallback, toolAccums map[string]*responsesToolAccum, inputTokens, outputTokens *int) responsesSSELineResult {
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return responsesSSELineResult{}
	}
	var evt struct {
		Type string `json:"type"`
		// output_text.delta
		Delta string `json:"delta"`
		// response.reasoning.delta (some Codex builds use "delta" too)
		Text string `json:"text"`
		// function_call output item
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		// response.completed / response.in_progress usage
		Response struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details,omitempty"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				// OpenAI Responses API emits input_tokens_details.cached_tokens
				// when the upstream prompt cache served part of the input.
				// Forwarding it lets the handler report real cache hits to the
				// client instead of the locally-simulated promptCacheTracker.
				InputTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details,omitempty"`
			} `json:"usage"`
		} `json:"response"`
		// item (for response.output_item.done with function_call)
		Item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		return responsesSSELineResult{err: fmt.Errorf("codex SSE parse: %w", err)}
	}

	switch evt.Type {
	case "response.output_text.delta":
		if evt.Delta != "" && callback.OnText != nil {
			callback.OnText(evt.Delta, false)
		}
	case "response.reasoning.delta":
		// Codex emits reasoning as "delta" or "text" depending on build.
		text := evt.Delta
		if text == "" {
			text = evt.Text
		}
		if text != "" && callback.OnText != nil {
			callback.OnText(text, true)
		}
	case "response.output_text.done":
		// Final text — already streamed via deltas. No-op.
	case "response.output_item.added":
		// A function_call item started — begin accumulating arguments.
		if evt.Item.Type == "function_call" && evt.Item.CallID != "" {
			toolAccums[evt.Item.CallID] = &responsesToolAccum{
				ID:   evt.Item.CallID,
				Name: evt.Item.Name,
			}
		}
	case "response.function_call_arguments.delta":
		if evt.CallID != "" {
			acc, ok := toolAccums[evt.CallID]
			if !ok {
				acc = &responsesToolAccum{ID: evt.CallID}
				toolAccums[evt.CallID] = acc
			}
			acc.Args.WriteString(evt.Delta)
		}
	case "response.output_item.done":
		// Final tool call — emit via callback.
		if evt.Item.Type == "function_call" && evt.Item.CallID != "" {
			acc, ok := toolAccums[evt.Item.CallID]
			if !ok {
				acc = &responsesToolAccum{ID: evt.Item.CallID, Name: evt.Item.Name}
			}
			args := acc.Args.String()
			if evt.Item.Arguments != "" {
				args = evt.Item.Arguments
			}
			name := evt.Item.Name
			if name == "" {
				name = acc.Name
			}
			var input map[string]interface{}
			if args != "" {
				_ = json.Unmarshal([]byte(args), &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			if callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: evt.Item.CallID,
					Name:      name,
					Input:     input,
				})
			}
			delete(toolAccums, evt.Item.CallID)
		}
	case "response.completed":
		if evt.Response.Usage != nil {
			*inputTokens = evt.Response.Usage.InputTokens
			*outputTokens = evt.Response.Usage.OutputTokens
			// Forward upstream prompt-cache hit so the handler reports real
			// cached_tokens to the client instead of the simulated tracker.
			if evt.Response.Usage.InputTokensDetails != nil &&
				evt.Response.Usage.InputTokensDetails.CachedTokens > 0 &&
				callback.OnCacheRead != nil {
				callback.OnCacheRead(evt.Response.Usage.InputTokensDetails.CachedTokens)
			}
		}
		if callback.OnStopReason != nil {
			callback.OnStopReason("end_turn")
		}
		return responsesSSELineResult{completed: true}
	case "response.incomplete":
		reason := ""
		if evt.Response.IncompleteDetails != nil {
			reason = evt.Response.IncompleteDetails.Reason
		}
		if reason == "max_output_tokens" || reason == "max_tokens" {
			if evt.Response.Usage != nil {
				*inputTokens = evt.Response.Usage.InputTokens
				*outputTokens = evt.Response.Usage.OutputTokens
			}
			if callback.OnStopReason != nil {
				callback.OnStopReason("max_tokens")
			}
			return responsesSSELineResult{completed: true}
		}
		return responsesSSELineResult{err: fmt.Errorf("codex stream response.incomplete: %s", truncateErrBody([]byte(data)))}
	case "response.failed", "error":
		// Return the error directly instead of only invoking OnError. Most
		// handlers rely on dispatchChat's return value to rotate accounts or
		// emit a terminal SSE error, and otherwise silently accept this stream.
		return responsesSSELineResult{err: fmt.Errorf("codex stream %s: %s", evt.Type, truncateErrBody([]byte(data)))}
	}
	return responsesSSELineResult{}
}

// parseResponsesJSON handles the rare non-SSE Responses response (when
// stream=true was ignored upstream). Reads a single Responses API JSON
// object and drives the callback.
func parseResponsesJSON(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("codex JSON read: %w", err)
	}
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details,omitempty"`
		} `json:"usage"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details,omitempty"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("codex JSON parse: %w", err)
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Text != "" && callback.OnText != nil {
					callback.OnText(c.Text, false)
				}
			}
		case "function_call":
			var input map[string]interface{}
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			if callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: item.CallID,
					Name:      item.Name,
					Input:     input,
				})
			}
		}
	}
	// Forward upstream prompt-cache hit (non-stream path). Fired
	// independently of OnComplete so a callback that only cares about
	// cache numbers still receives them.
	if resp.Usage != nil &&
		resp.Usage.InputTokensDetails != nil &&
		resp.Usage.InputTokensDetails.CachedTokens > 0 &&
		callback.OnCacheRead != nil {
		callback.OnCacheRead(resp.Usage.InputTokensDetails.CachedTokens)
	}
	stopReason := "end_turn"
	if strings.EqualFold(resp.Status, "incomplete") && resp.IncompleteDetails != nil {
		if resp.IncompleteDetails.Reason == "max_output_tokens" || resp.IncompleteDetails.Reason == "max_tokens" {
			stopReason = "max_tokens"
		} else {
			return fmt.Errorf("codex JSON response incomplete: %s", resp.IncompleteDetails.Reason)
		}
	}
	if callback.OnStopReason != nil {
		callback.OnStopReason(stopReason)
	}
	if callback.OnComplete != nil {
		in, out := 0, 0
		if resp.Usage != nil {
			in = resp.Usage.InputTokens
			out = resp.Usage.OutputTokens
		}
		callback.OnComplete(in, out)
	}
	return nil
}
