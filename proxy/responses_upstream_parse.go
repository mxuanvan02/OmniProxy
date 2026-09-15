package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

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
			return fmt.Errorf("responses SSE read: %w", err)
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
		return responsesSSELineResult{err: fmt.Errorf("responses SSE parse: %w", err)}
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
		return responsesSSELineResult{err: fmt.Errorf("responses stream response.incomplete: %s", truncateErrBody([]byte(data)))}
	case "response.failed", "error":
		// Return the error directly instead of only invoking OnError. Most
		// handlers rely on dispatchChat's return value to rotate accounts or
		// emit a terminal SSE error, and otherwise silently accept this stream.
		return responsesSSELineResult{err: fmt.Errorf("responses stream %s: %s", evt.Type, truncateErrBody([]byte(data)))}
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
		return fmt.Errorf("responses JSON read: %w", err)
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
		return fmt.Errorf("responses JSON parse: %w", err)
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
			return fmt.Errorf("responses JSON response incomplete: %s", resp.IncompleteDetails.Reason)
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
