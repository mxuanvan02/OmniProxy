// Package proxy — Anthropic Messages SSE read loop for external accounts.
//
// The event vocabulary differs from the OpenAI dialect in one way that shapes
// this parser: a tool call arrives whole. Its arguments stream as
// input_json_delta fragments belonging to one block, the block closes with
// content_block_stop, and only then is the call complete — so one accumulator
// per block index is enough and no index-to-call mapping is needed.
package proxy

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"omniproxy/logger"
)

// normalizeAnthropicStopReason maps the Messages API's terminal names onto the
// vocabulary the Claude adapter emits. A stop sequence becomes end_turn: the
// turn finished normally, and the sequence itself is not part of the payload.
func normalizeAnthropicStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "end_turn", "stop_sequence":
		return "end_turn"
	case "max_tokens":
		return "max_tokens"
	case "tool_use":
		return "tool_use"
	default:
		return normalizeUpstreamStopReason(reason)
	}
}

// anthropicBlockAccum is one open content block. Text blocks need nothing
// beyond their kind, and a tool_use block is not complete until it stops, so
// the arguments it streams are held here rather than forwarded.
type anthropicBlockAccum struct {
	kind        string
	id          string
	name        string
	partialJSON string
}

// parseExternalAnthropicSSE reads a Messages API event stream and emits
// KiroStreamCallback events.
func parseExternalAnthropicSSE(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	// Withhold whitespace-only text so a turn that never produces real content
	// stays retryable instead of reaching the client as a finished, empty
	// answer.
	gate := newBlankOutputGate(callback)
	callback = gate.callback()

	var watchdog *sseIdleWatchdog
	if rc, ok := body.(io.ReadCloser); ok {
		watchdog = newSSEIdleWatchdog(rc)
		if watchdog != nil {
			watchdog.Start()
			defer watchdog.Stop()
		}
	}

	state := newAnthropicStreamState()
	// Blank-stream diagnostics: keep the first few raw payloads (truncated) so
	// the next "ended without assistant output" names the dialect the gateway
	// actually sent instead of guessing. Bounded to avoid unbounded memory.
	var blankSamples []string
	terminal := false
	stopReason := ""

	reader := bufio.NewReaderSize(body, 16*1024)
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			state.events++
			if len(blankSamples) < 5 && data != "" {
				sample := data
				if len(sample) > 300 {
					sample = sample[:300] + "...[truncated]"
				}
				blankSamples = append(blankSamples, sample)
			}
			if data == "[DONE]" {
				terminal = true
			} else {
				result, parseErr := parseAnthropicSSEEvent(data, callback, state)
				if parseErr != nil {
					return parseErr
				}
				recordExternalSSEProgress(watchdog, result)
				if result.terminal {
					terminal = true
				}
				if result.stopReason != "" {
					stopReason = result.stopReason
				}
			}
		}
		if readErr != nil {
			if watchdog != nil && watchdog.TimedOut() {
				return ErrStreamIdleTimeout
			}
			if readErr != io.EOF {
				return fmt.Errorf("external anthropic SSE read: %w", readErr)
			}
			break
		}
		if terminal {
			break
		}
	}

	// Some gateways (e.g. VIBE7-style Anthropic-compatible proxies) close the
	// HTTP connection after the last delta without emitting message_stop. When
	// real assistant output was already streamed, recover the partial turn
	// instead of discarding it — otherwise long generations surface as hard
	// failures even though the model produced content.
	if !terminal {
		if state.sawOutput && gate.meaningful {
			if stopReason == "" {
				stopReason = "end_turn"
			}
			if callback.OnStopReason != nil {
				callback.OnStopReason(stopReason)
			}
			if callback.OnComplete != nil {
				callback.OnComplete(state.inputTokens, state.outputTokens)
			}
			return nil
		}
		return fmt.Errorf("external anthropic SSE stream ended before message_stop")
	}
	if !state.sawOutput {
		logger.Warnf("[ExternalAnthropic] blank SSE without assistant output: events=%d samples=%q", state.events, blankSamples)
		return fmt.Errorf("external anthropic SSE stream ended without assistant output")
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	// A stream can be well-formed and still carry nothing renderable. Report it
	// so the caller can rotate accounts rather than closing the turn empty.
	if !gate.meaningful {
		logger.Warnf("[ExternalAnthropic] blank turn (gate not meaningful): events=%d samples=%q stopReason=%q", state.events, blankSamples, stopReason)
		return blankTurnError(stopReason)
	}
	if callback.OnStopReason != nil {
		callback.OnStopReason(stopReason)
	}
	if callback.OnComplete != nil {
		callback.OnComplete(state.inputTokens, state.outputTokens)
	}
	return nil
}
