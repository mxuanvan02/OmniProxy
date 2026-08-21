// Package proxy — external OpenAI-compatible provider adapter.
//
// Accounts with AuthMethod == "external_openai" forward chat-completion
// requests to an upstream OpenAI-compatible endpoint ({BaseURL}/v1/chat/completions)
// using the account's AccessToken as the Bearer key. The KiroPayload intermediate
// representation (produced by ClaudeToKiro / OpenAIToKiro) is translated back to
// the OpenAI chat-completions request shape, and the upstream SSE/JSON response
// is replayed through the same KiroStreamCallback interface used by CallKiroAPI,
// so the existing Claude/OpenAI response handlers work unchanged.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"regexp"
	"strings"
	"time"
)

// externalAuthMethod is the AuthMethod value marking an external OpenAI-compatible provider.
const externalAuthMethod = "external_openai"

// ErrExternalCreditsNotSupported is returned by fetchExternalProviderCredits
// when the upstream provider does not implement /api/me (HTTP 404). Callers
// treat this as a non-fatal "no credit info available" condition rather than
// a hard error, so the refresh button stays green even for providers that
// only expose /v1/* endpoints.
var ErrExternalCreditsNotSupported = fmt.Errorf("provider does not expose /api/me (credits API not supported)")

// isExternalAccount reports whether the account routes to an external
// OpenAI-compatible or AgentRouter provider instead of the native Kiro/AWS backend.
func isExternalAccount(account *config.Account) bool {
	if account == nil {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(account.AuthMethod))
	return m == externalAuthMethod || m == "agentrouter" || m == "external_agentrouter"
}

// CallExternalOpenAI forwards a KiroPayload to an external OpenAI-compatible
// provider and replays the response through callback. It mirrors CallKiroAPI's
// contract: it returns nil on a successful (possibly empty) stream, or an error
// describing the failure. HTTP 401/403/402 are returned directly so the caller's
// existing auth-failure handling can disable the account.
func CallExternalOpenAI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("external call: account is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("external account %s has no baseUrl", account.Email)
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return fmt.Errorf("external account %s has no apiKey", account.Email)
	}

	body, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		return fmt.Errorf("external call build request: %w", err)
	}
	// Always request a stream from the upstream; the handler's non-stream path
	// already buffers via the callback. We still tolerate a non-SSE response
	// (some providers ignore stream=true) by sniffing the Content-Type.
	body["stream"] = true
	// Request usage in the terminal chunk when the provider supports it.
	body["stream_options"] = map[string]bool{"include_usage": true}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("external call marshal: %w", err)
	}

	endpoint := openAICompatibleEndpoint(baseURL, "/v1/chat/completions")
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("external call new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("external call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
		// Auth/payment errors are not retried across endpoints — surface them
		// so the pool's auth-failure handling can disable the account.
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
			return err
		}
		return err
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseExternalOpenAISSE(resp.Body, callback)
	}
	// Provider ignored stream=true and returned a single JSON object.
	return parseExternalOpenAIJSON(resp.Body, callback)
}

func truncateErrBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	const max = 400
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// kiroPayloadToOpenAIRequest converts the KiroPayload intermediate representation
// back into an OpenAI chat-completions request body (map form, to be marshalled
// by the caller). System priming injected by OpenAIToKiro/ClaudeToKiro as the
// first history pair (user system prompt → assistant "I will follow...") is
// collapsed back into a single system message so the external provider sees a
// conventional chat-completions transcript.
func kiroPayloadToOpenAIRequest(payload *KiroPayload, account *config.Account) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("nil payload")
	}

	modelID := strings.TrimSpace(payload.OriginalModel)
	if modelID == "" {
		// Fallback to the Kiro-mapped model ID if the caller didn't preserve
		// the original. This is suboptimal for external providers (they'd
		// receive a Kiro alias like "claude-sonnet-4.5" instead of "gpt-4o")
		// but never empty.
		modelID = strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	}
	if modelID == "" {
		modelID = "auto"
	}
	// Strip internal routing prefixes that external providers don't understand.
	// Kiro accounts advertise models with a "kr/" prefix (and OmniProxy uses
	// "omniproxy/" internally for combo routing); external OpenAI-compatible
	// providers receive the bare model ID so their model registry can match it.
	modelID = stripInternalModelPrefix(modelID)
	// External providers (e.g. bddevlab) use dash-form model IDs
	// ("claude-opus-4-8") while OmniProxy's ParseModelAndThinking normalises
	// to dot-form ("claude-opus-4.8"). Revert to dash-form so the external
	// provider's model registry can match. Only applies to claude-* models;
	// other model families (gpt-*, o1-*, etc.) pass through unchanged.
	modelID = dotToDashClaudeVersion(modelID)
	modelID = applyExternalModelMapping(account, modelID)
	// Do not discover models on the inference hot path. A provider's /v1/models
	// endpoint is optional and may be slow or unavailable even when
	// /v1/chat/completions is healthy. Resolving a model here would delay every
	// request by the discovery timeout and can make Claude CLI appear to stop
	// before the assistant/tool stream starts. Model discovery remains an
	// explicit admin/cache operation in fetchAndCacheAccountModels and
	// apiGetAccountModels.

	msgs := make([]map[string]interface{}, 0, 8)
	history := payload.ConversationState.History

	// Detect & extract the system priming pair injected by the translators:
	//   history[0] = user(systemPrompt), history[1] = assistant("I will follow...")
	systemPrompt := ""
	if len(history) >= 2 {
		first := history[0]
		second := history[1]
		if first.UserInputMessage != nil && second.AssistantResponseMessage != nil &&
			strings.Contains(strings.ToLower(strings.TrimSpace(second.AssistantResponseMessage.Content)), "i will follow") {
			systemPrompt = strings.TrimSpace(first.UserInputMessage.Content)
			history = history[2:]
		}
	}

	if systemPrompt != "" {
		msgs = append(msgs, map[string]interface{}{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	for _, h := range history {
		if h.UserInputMessage != nil {
			um := h.UserInputMessage
			// A history user message may carry tool results (tool turn). Emit
			// them as separate "tool" messages followed by any user text.
			if um.UserInputMessageContext != nil && len(um.UserInputMessageContext.ToolResults) > 0 {
				for _, tr := range um.UserInputMessageContext.ToolResults {
					text := ""
					if len(tr.Content) > 0 {
						text = tr.Content[0].Text
					}
					msgs = append(msgs, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": tr.ToolUseID,
						"content":      text,
					})
				}
			}
			if strings.TrimSpace(um.Content) != "" || len(um.Images) > 0 {
				msgs = append(msgs, map[string]interface{}{
					"role":    "user",
					"content": openAIUserContent(um.Content, um.Images),
				})
			}
		} else if h.AssistantResponseMessage != nil {
			am := h.AssistantResponseMessage
			m := map[string]interface{}{
				"role":    "assistant",
				"content": am.Content,
			}
			if len(am.ToolUses) > 0 {
				tcs := make([]map[string]interface{}, 0, len(am.ToolUses))
				for _, tu := range am.ToolUses {
					args, _ := json.Marshal(tu.Input)
					tcs = append(tcs, map[string]interface{}{
						"id":   tu.ToolUseID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      restoreToolName(payload, tu.Name),
							"arguments": string(args),
						},
					})
				}
				m["tool_calls"] = tcs
			}
			msgs = append(msgs, m)
		}
	}

	// Current user message.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		for _, tr := range cur.UserInputMessageContext.ToolResults {
			text := ""
			if len(tr.Content) > 0 {
				text = tr.Content[0].Text
			}
			msgs = append(msgs, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": tr.ToolUseID,
				"content":      text,
			})
		}
	}
	if strings.TrimSpace(cur.Content) != "" || len(cur.Images) > 0 {
		msgs = append(msgs, map[string]interface{}{
			"role":    "user",
			"content": openAIUserContent(cur.Content, cur.Images),
		})
	}

	body := map[string]interface{}{
		"model":    modelID,
		"messages": msgs,
	}

	// When CacheControlPassthrough is enabled, attach Anthropic-style
	// cache_control breakpoints at stable prefix boundaries so upstream
	// providers that honour prompt caching (e.g. Anthropic via OpenAI-
	// compatible gateways) can cache the durable system prompt and conversation
	// history. The current user turn is deliberately excluded: it changes on the
	// next request and otherwise creates cache-write churn with little reuse.
	if config.GetCacheControlPassthrough() && len(msgs) > 0 {
		applyExternalCacheControl(msgs)
	}

	// Tools — restore original (pre-sanitization) names for the external provider.
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(cur.UserInputMessageContext.Tools))
		for _, tw := range cur.UserInputMessageContext.Tools {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        restoreToolName(payload, tw.ToolSpecification.Name),
					"description": tw.ToolSpecification.Description,
					"parameters":  sanitizeExternalToolSchema(tw.ToolSpecification.InputSchema.JSON),
				},
			})
		}
		body["tools"] = tools
	}
	if choice := openAIToolChoice(payload.ToolChoice, payload); choice != nil {
		body["tool_choice"] = choice
	}

	if payload.InferenceConfig != nil {
		if payload.InferenceConfig.MaxTokens > 0 {
			body["max_tokens"] = payload.InferenceConfig.MaxTokens
		}
		if payload.InferenceConfig.Temperature > 0 {
			body["temperature"] = payload.InferenceConfig.Temperature
		}
		if payload.InferenceConfig.TopP > 0 {
			body["top_p"] = payload.InferenceConfig.TopP
		}
		// Pass reasoning_effort to OpenAI-compatible upstreams (e.g. gpt-5.6-sol)
		// when the client requested thinking. This enables the upstream model to
		// allocate reasoning budget instead of relying only on the system prompt.
		if payload.InferenceConfig.ReasoningEffort != "" {
			body["reasoning_effort"] = payload.InferenceConfig.ReasoningEffort
		}
	}

	return body, nil
}

// applyExternalModelMapping rewrites a public model ID to the model ID used by
// one external account. Matching is case-insensitive so hand-edited config is
// not sensitive to model-ID casing.
func applyExternalModelMapping(account *config.Account, modelID string) string {
	if account == nil || len(account.ModelMappings) == 0 {
		return modelID
	}
	for source, target := range account.ModelMappings {
		if strings.EqualFold(strings.TrimSpace(source), modelID) {
			if target = strings.TrimSpace(target); target != "" {
				return target
			}
			return modelID
		}
	}
	return modelID
}

// sanitizeExternalToolSchema removes enum constraints that Gemini-compatible
// gateways reject when an enum member is not a string. The property's type is
// preserved, so boolean and numeric inputs remain correctly typed. A deep copy
// keeps the Kiro payload reusable by other upstreams without mutation.
func sanitizeExternalToolSchema(schema interface{}) interface{} {
	cloned := cloneSchemaValue(schema)
	cleanExternalToolSchema(cloned)
	return cloned
}

func cleanExternalToolSchema(value interface{}) {
	switch current := value.(type) {
	case map[string]interface{}:
		if enum, exists := current["enum"]; exists && !isNonEmptyStringEnum(enum) {
			delete(current, "enum")
		}
		for _, child := range current {
			cleanExternalToolSchema(child)
		}
	case []interface{}:
		for _, child := range current {
			cleanExternalToolSchema(child)
		}
	}
}

func isNonEmptyStringEnum(value interface{}) bool {
	switch enum := value.(type) {
	case []interface{}:
		if len(enum) == 0 {
			return false
		}
		for _, member := range enum {
			if _, ok := member.(string); !ok {
				return false
			}
		}
		return true
	case []string:
		return len(enum) > 0
	default:
		return false
	}
}

// openAIToolChoice converts Anthropic's tool_choice vocabulary to the
// OpenAI-compatible vocabulary used by external chat-completions providers.
// A nil choice is intentionally omitted so upstream keeps its default auto
// behavior for ordinary turns.
func openAIToolChoice(choice interface{}, payload *KiroPayload) interface{} {
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
				"function": map[string]interface{}{
					"name": restoreToolName(payload, name),
				},
			}
		}
	}
	return choice
}

func cloneToolChoice(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var clone interface{}
	if err := json.Unmarshal(data, &clone); err != nil {
		return value
	}
	return clone
}

// applyExternalCacheControl attaches Anthropic-style cache_control breakpoints
// to up to 4 stable prefix boundaries in the OpenAI chat messages list:
//
//  1. The system message (if present) — the most stable, highest-value prefix.
//  2. The last non-tool history message before the current turn.
//
// The field is added in-place to the message maps. Providers that
// do not understand cache_control will ignore it (OpenAI) or reject it — the
// operator is responsible for enabling this only for providers that honour it.
//
// The breakpoint uses the default 5-minute ephemeral TTL, matching Anthropic's
// default. A 1h TTL is not exposed here because OpenAI-compatible gateways
// rarely surface the ttl sub-field.
func applyExternalCacheControl(msgs []map[string]interface{}) {
	const maxBreakpoints = 4
	breakpoints := 0
	cacheControl := map[string]interface{}{"type": "ephemeral"}

	// 1) System message (first message with role=system).
	if breakpoints < maxBreakpoints {
		for _, m := range msgs {
			if role, _ := m["role"].(string); role == "system" {
				m["cache_control"] = cacheControl
				breakpoints++
				break
			}
		}
	}

	// 2) Last message before the current user turn — i.e. the last message
	//    that is NOT the final user/tool message. This is the conversation
	//    history tail, the second most stable prefix.
	if breakpoints < maxBreakpoints && len(msgs) >= 3 {
		lastHistoryIdx := len(msgs) - 2 // second-to-last
		if lastHistoryIdx > 0 {
			if role, _ := msgs[lastHistoryIdx]["role"].(string); role != "system" {
				msgs[lastHistoryIdx]["cache_control"] = cacheControl
				breakpoints++
			}
		}
	}

	// 3) Current user message. This creates a cache breakpoint at the end of
	// the stable prefix while leaving interior history messages untouched.
	if breakpoints < maxBreakpoints && len(msgs) >= 3 {
		currentIdx := len(msgs) - 1
		if role, _ := msgs[currentIdx]["role"].(string); role == "user" {
			msgs[currentIdx]["cache_control"] = cacheControl
			breakpoints++
		}
	}

}

func openAIUserContent(text string, images []KiroImage) interface{} {
	if len(images) == 0 {
		return text
	}

	content := make([]map[string]interface{}, 0, len(images)+1)
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}
	for _, image := range images {
		format := strings.TrimSpace(image.Format)
		if format == "" {
			format = "png"
		}
		content = append(content, map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]interface{}{
				"url": "data:image/" + format + ";base64," + image.Source.Bytes,
			},
		})
	}
	return content
}

// resolveExternalModelID picks the best matching model ID from the provider's
// cached model list. External providers may use slightly different naming
// conventions (e.g. "claude-haiku-4-5-20251001" with a date suffix instead of
// "claude-haiku-4-5"). When the requested ID isn't an exact match, we try a
// prefix match against the cached list. Falls back to the input ID when no
// cache is available or no match is found.
func resolveExternalModelID(account *config.Account, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return requested
	}
	models, err := fetchExternalProviderModels(account)
	if err != nil || len(models) == 0 {
		return requested
	}
	// Exact match → use as-is.
	for _, m := range models {
		if strings.EqualFold(m.ModelId, requested) {
			return requested
		}
	}
	// Prefix match: requested "claude-haiku-4-5" matches "claude-haiku-4-5-20251001".
	// Pick the shortest matching ID (closest to what was requested).
	best := ""
	for _, m := range models {
		if strings.HasPrefix(strings.ToLower(m.ModelId), strings.ToLower(requested)) {
			if best == "" || len(m.ModelId) < len(best) {
				best = m.ModelId
			}
		}
	}
	if best != "" {
		logger.Infof("[ExternalOpenAI] model %q not exact, resolved to %q via prefix match", requested, best)
		return best
	}
	return requested
}

// dotToDashClaudeVersion reverts OmniProxy's dot-form normalisation
// ("claude-opus-4.8" → "claude-opus-4-8") for external providers that use
// dash-form model IDs. Non-claude models and already-dash-form IDs pass
// through unchanged. Dated snapshots (claude-sonnet-4-20250514) are not
// modified because they never went through dot normalisation.
func dotToDashClaudeVersion(model string) string {
	if !strings.HasPrefix(strings.ToLower(model), "claude-") {
		return model
	}
	// claude-{family}-{N}.{M} → claude-{family}-{N}-{M}
	// Mirror of claudeVersionPattern in translator.go but reversed.
	return claudeDotToDashPattern.ReplaceAllString(model, "claude-$1-$2-$3")
}

var claudeDotToDashPattern = regexp.MustCompile(`(?i)claude-(opus|sonnet|haiku)-(\d+)\.(\d{1,2})\b`)

// stripInternalModelPrefix removes routing prefixes that OmniProxy / Kiro use
// internally but external OpenAI-compatible providers don't understand:
//   - "kr/"        — Kiro account model prefix (e.g. "kr/claude-sonnet-5")
//   - "omniproxy/" — OmniProxy combo routing prefix
//
// The bare model ID is returned so the external provider's own model registry
// can match it (e.g. "claude-sonnet-5"). Unknown prefixes pass through unchanged.
func stripInternalModelPrefix(model string) string {
	for _, p := range []string{"kr/", "omniproxy/"} {
		if strings.HasPrefix(model, p) {
			return strings.TrimPrefix(model, p)
		}
	}
	return model
}

// restoreToolName maps a sanitized tool name back to the original client-supplied
// name using the payload's ToolNameMap. Falls back to the sanitized name.
func restoreToolName(payload *KiroPayload, name string) string {
	if payload != nil && payload.ToolNameMap != nil {
		if orig, ok := payload.ToolNameMap[name]; ok && orig != "" {
			return orig
		}
	}
	return name
}

// restoreCallbackToolNames canonicalizes upstream tool calls before they reach
// protocol-specific response handlers. In particular, Codex may return the
// sanitized names it received (for example "read" instead of Claude's
// case-sensitive "Read"). Unknown names pass through unchanged.
func restoreCallbackToolNames(payload *KiroPayload, callback *KiroStreamCallback) *KiroStreamCallback {
	if callback == nil || callback.OnToolUse == nil || payload == nil || len(payload.ToolNameMap) == 0 {
		return callback
	}

	wrapped := *callback
	originalOnToolUse := callback.OnToolUse
	wrapped.OnToolUse = func(tu KiroToolUse) {
		tu.Name = restoreToolName(payload, tu.Name)
		originalOnToolUse(tu)
	}
	return &wrapped
}

// ==================== SSE parsing ====================

type externalToolAccum struct {
	ID        string
	Name      string
	Arguments string
}

// normalizeUpstreamStopReason maps the terminal names used by the upstream
// providers to the values expected by the Claude adapter.
func normalizeUpstreamStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "stop", "completed", "end_turn":
		return "end_turn"
	case "length", "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return strings.ToLower(strings.TrimSpace(reason))
	}
}

type externalSSELineResult struct {
	recognized       bool
	recognizedOutput bool
	terminal         bool
	stopReason       string
	err              error
}

// externalSSEProviderError identifies an explicit error object carried inside
// an otherwise HTTP-200 SSE response. The observation flags are parser-owned,
// so retry safety does not depend on whether the caller supplied callbacks.
type externalSSEProviderError struct {
	message            string
	priorEventObserved bool
	outputObserved     bool
}

func (e *externalSSEProviderError) Error() string {
	return "external provider error: " + e.message
}

// parseExternalOpenAISSE reads an OpenAI streaming chat-completion SSE stream
// and emits KiroStreamCallback events.
func parseExternalOpenAISSE(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	br := bufio.NewReaderSize(body, 16*1024)

	// SSE idle watchdog — same rationale as parseCodexResponsesSSE.
	var watchdog *sseIdleWatchdog
	if rc, ok := body.(io.ReadCloser); ok {
		watchdog = newSSEIdleWatchdog(rc)
		if watchdog != nil {
			watchdog.Start()
			defer watchdog.Stop()
		}
	}

	var inputTokens, outputTokens int
	toolAccums := map[int]*externalToolAccum{}
	var toolOrder []int
	sawDataEvent := false
	sawAssistantOutput := false
	terminal := false
	stopReason := ""

	emitToolCalls := func() {
		for _, idx := range toolOrder {
			tc := toolAccums[idx]
			if tc == nil || tc.Name == "" {
				continue
			}
			var input map[string]interface{}
			if strings.TrimSpace(tc.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
					// Fall back to a raw wrapper so the client still sees the args.
					input = map[string]interface{}{"_raw": tc.Arguments}
				}
			}
			if input == nil {
				input = make(map[string]interface{})
			}
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", idx)
			}
			if callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: id,
					Name:      tc.Name,
					Input:     input,
				})
			}
		}
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if watchdog != nil && watchdog.TimedOut() {
				return ErrStreamIdleTimeout
			}
			if err == io.EOF {
				if strings.TrimSpace(line) != "" {
					// ReadString returns the final unterminated line together with
					// io.EOF. Handle inline provider errors here as well, otherwise
					// a clean pre-output error cannot trigger AgentRouter fallback.
					dataLine := strings.TrimRight(line, "\r\n")
					if strings.HasPrefix(dataLine, "data:") {
						data := strings.TrimSpace(strings.TrimPrefix(dataLine, "data:"))
						if data != "[DONE]" {
							if errMsg := extractExternalSSEError([]byte(data)); errMsg != "" {
								return &externalSSEProviderError{
									message:            errMsg,
									priorEventObserved: sawDataEvent,
									outputObserved:     sawAssistantOutput,
								}
							}
						}
					}
					result := processExternalSSELine(line, callback, toolAccums, &toolOrder, &inputTokens, &outputTokens)
					if result.err != nil {
						return result.err
					}
					sawDataEvent = sawDataEvent || result.recognized
					sawAssistantOutput = sawAssistantOutput || result.recognizedOutput
					terminal = terminal || result.terminal
					if result.stopReason != "" {
						stopReason = result.stopReason
					}
				}
				break
			}
			return fmt.Errorf("external SSE read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		// Real data line — reset the SSE idle watchdog timer.
		if watchdog != nil {
			watchdog.DataReceived()
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			terminal = true
			if stopReason == "" {
				stopReason = "end_turn"
			}
			break
		}

		// Some providers (e.g. mrdev.cyou) return HTTP 200 with an inline
		// SSE error event: data: {"error":{"message":"Invalid request"}}
		// when a model is unavailable or rejects the request. Detect this
		// and surface it as an error so the combo handler can fall back to
		// the next model instead of treating the empty stream as success.
		if errMsg := extractExternalSSEError([]byte(data)); errMsg != "" {
			emitToolCalls()
			return &externalSSEProviderError{
				message:            errMsg,
				priorEventObserved: sawDataEvent,
				outputObserved:     sawAssistantOutput,
			}
		}
		result := processExternalSSEData(data, callback, toolAccums, &toolOrder, &inputTokens, &outputTokens)
		if result.err != nil {
			return result.err
		}
		sawDataEvent = sawDataEvent || result.recognized
		sawAssistantOutput = sawAssistantOutput || result.recognizedOutput
		terminal = terminal || result.terminal
		if result.stopReason != "" {
			stopReason = result.stopReason
		}
	}
	if !terminal {
		return fmt.Errorf("external SSE stream ended before a terminal finish_reason or [DONE]")
	}
	if !sawAssistantOutput {
		return fmt.Errorf("external SSE stream ended without assistant output")
	}

	emitToolCalls()
	if stopReason == "" {
		stopReason = "end_turn"
	}
	if callback.OnStopReason != nil {
		callback.OnStopReason(stopReason)
	}
	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

// processExternalSSELine handles a single non-empty SSE data line, including
// the final line returned together with io.EOF.
func processExternalSSELine(line string, callback *KiroStreamCallback, toolAccums map[int]*externalToolAccum, toolOrder *[]int, inputTokens, outputTokens *int) externalSSELineResult {
	if !strings.HasPrefix(line, "data:") {
		return externalSSELineResult{}
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		return externalSSELineResult{recognized: true, terminal: true, stopReason: "end_turn"}
	}
	return processExternalSSEData(data, callback, toolAccums, toolOrder, inputTokens, outputTokens)
}

func processExternalSSEData(data string, callback *KiroStreamCallback, toolAccums map[int]*externalToolAccum, toolOrder *[]int, inputTokens, outputTokens *int) externalSSELineResult {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return externalSSELineResult{err: fmt.Errorf("external SSE parse: %w", err)}
	}
	sawOutput := false
	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" {
			sawOutput = true
			if callback.OnOutput != nil {
				callback.OnOutput()
			}
			if callback.OnText != nil {
				callback.OnText(ch.Delta.Content, false)
			}
		}
		if ch.Delta.ReasoningContent != "" {
			sawOutput = true
			if callback.OnOutput != nil {
				callback.OnOutput()
			}
			if callback.OnText != nil {
				callback.OnText(ch.Delta.ReasoningContent, true)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			// A tool call is assistant output even when the text delta is empty.
			sawOutput = true
			if callback.OnOutput != nil {
				callback.OnOutput()
			}
			acc, ok := toolAccums[tc.Index]
			if !ok {
				acc = &externalToolAccum{}
				toolAccums[tc.Index] = acc
				*toolOrder = append(*toolOrder, tc.Index)
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Name = tc.Function.Name
			}
			acc.Arguments += tc.Function.Arguments
		}
	}
	if chunk.Usage != nil {
		*inputTokens = chunk.Usage.PromptTokens
		*outputTokens = chunk.Usage.CompletionTokens
		if chunk.Usage.PromptTokensDetails != nil &&
			chunk.Usage.PromptTokensDetails.CachedTokens > 0 && callback.OnCacheRead != nil {
			callback.OnCacheRead(chunk.Usage.PromptTokensDetails.CachedTokens)
		}
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != "" {
			return externalSSELineResult{
				recognized:       true,
				recognizedOutput: sawOutput,
				terminal:         true,
				stopReason:       normalizeUpstreamStopReason(ch.FinishReason),
			}
		}
	}
	return externalSSELineResult{
		recognized:       true,
		recognizedOutput: sawOutput,
	}
}

// extractExternalSSEError checks whether a JSON SSE data payload is an
// inline error event ({"error":{"message":"..."}} or {"detail":"..."}).
// Returns the error message if so, or empty string if the payload is a
// normal content/usage chunk. Some providers (e.g. mrdev.cyou) return
// HTTP 200 with an error embedded in the SSE stream when a model rejects
// the request; without this check the empty stream is treated as success
// and combo fallback never triggers.
func extractExternalSSEError(data []byte) string {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	if probe.Error != nil && probe.Error.Message != "" {
		return probe.Error.Message
	}
	if probe.Detail != "" {
		return probe.Detail
	}
	return ""
}

// parseExternalOpenAIJSON handles a non-streaming JSON response from a provider
// that ignored stream=true. Emits the same callback events as the SSE parser.
func parseExternalOpenAIJSON(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("external json read: %w", err)
	}
	// Detect inline error payloads ({"error":{"message":"..."}} or
	// {"detail":"..."}) that some providers return with HTTP 200.
	if errMsg := extractExternalSSEError(data); errMsg != "" {
		return fmt.Errorf("external provider error: %s", errMsg)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("external json parse: %w", err)
	}

	sawAssistantOutput := false
	for _, ch := range resp.Choices {
		if ch.Message.Content != "" {
			sawAssistantOutput = true
			if callback.OnOutput != nil {
				callback.OnOutput()
			}
			if callback.OnText != nil {
				callback.OnText(ch.Message.Content, false)
			}
		}
		if ch.Message.ReasoningContent != "" {
			sawAssistantOutput = true
			if callback.OnOutput != nil {
				callback.OnOutput()
			}
			if callback.OnText != nil {
				callback.OnText(ch.Message.ReasoningContent, true)
			}
		}
		for _, tc := range ch.Message.ToolCalls {
			sawAssistantOutput = true
			if callback.OnOutput != nil {
				callback.OnOutput()
			}
			var input map[string]interface{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					input = map[string]interface{}{"_raw": tc.Function.Arguments}
				}
			}
			if input == nil {
				input = make(map[string]interface{})
			}
			id := tc.ID
			if id == "" {
				id = "call_" + tc.Function.Name
			}
			if callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: id,
					Name:      tc.Function.Name,
					Input:     input,
				})
			}
		}
	}

	var inTok, outTok int
	if resp.Usage != nil {
		inTok = resp.Usage.PromptTokens
		outTok = resp.Usage.CompletionTokens
		// Forward upstream native cache hit (non-stream path).
		if resp.Usage.PromptTokensDetails != nil &&
			resp.Usage.PromptTokensDetails.CachedTokens > 0 &&
			callback.OnCacheRead != nil {
			callback.OnCacheRead(resp.Usage.PromptTokensDetails.CachedTokens)
		}
	}
	if !sawAssistantOutput {
		return fmt.Errorf("external JSON response ended without assistant output")
	}
	stopReason := "end_turn"
	for _, ch := range resp.Choices {
		if ch.FinishReason != "" {
			stopReason = normalizeUpstreamStopReason(ch.FinishReason)
			break
		}
	}
	if callback.OnStopReason != nil {
		callback.OnStopReason(stopReason)
	}
	if callback.OnComplete != nil {
		callback.OnComplete(inTok, outTok)
	}
	return nil
}

// fetchExternalProviderModels lists models from an external OpenAI-compatible
// provider's /v1/models endpoint. Used by the admin "refresh models" path so
// external accounts show a real model list instead of the optimistic default.
func fetchExternalProviderModels(account *config.Account) ([]ModelInfo, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("no baseUrl")
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return nil, fmt.Errorf("no apiKey")
	}

	req, err := http.NewRequest("GET", openAICompatibleEndpoint(baseURL, "/v1/models"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client := GetRestClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(b))
	}

	var result struct {
		Data []struct {
			ID               string      `json:"id"`
			OwnedBy          string      `json:"owned_by,omitempty"`
			Modalities       interface{} `json:"modalities,omitempty"`
			InputModalities  interface{} `json:"input_modalities,omitempty"`
			OutputModalities interface{} `json:"output_modalities,omitempty"`
			Capabilities     interface{} `json:"capabilities,omitempty"`
			Type             string      `json:"type,omitempty"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID == "" {
			continue
		}
		provider := strings.TrimSpace(m.OwnedBy)
		if provider == "" {
			provider = "external"
		}
		out = append(out, ModelInfo{
			ModelId:     m.ID,
			ModelName:   m.ID,
			Provider:    provider,
			Modalities:  flattenModelMetadata(m.Modalities),
			OutputTypes: append(flattenModelMetadata(m.OutputModalities), imageCapabilityMetadata(m.Capabilities)...),
		})
	}
	return out, nil
}

// openAICompatibleEndpoint accepts either a provider root URL or a URL that
// already ends in /v1. This keeps all OpenAI-compatible adapters from
// producing invalid paths such as /v1/v1/models.
func openAICompatibleEndpoint(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return baseURL + path
}

func flattenModelMetadata(value interface{}) []string {
	var out []string
	var visit func(interface{})
	visit = func(current interface{}) {
		switch item := current.(type) {
		case string:
			if value := strings.TrimSpace(item); value != "" {
				out = append(out, value)
			}
		case []interface{}:
			for _, child := range item {
				visit(child)
			}
		case map[string]interface{}:
			for key, child := range item {
				visit(key)
				visit(child)
			}
		}
	}
	visit(value)
	return out
}

func imageCapabilityMetadata(value interface{}) []string {
	values := flattenModelMetadata(value)
	for _, item := range values {
		lower := strings.ToLower(item)
		if strings.Contains(lower, "image") && (strings.Contains(lower, "output") || strings.Contains(lower, "generate") || lower == "image") {
			return []string{"image output"}
		}
	}
	return nil
}

// dispatchChat routes a chat request to the appropriate upstream based on the
// account's auth method. Native Kiro/AWS accounts use CallKiroAPI; external
// OpenAI-compatible providers use CallExternalOpenAI. All response handlers
// (Claude/OpenAI, stream/non-stream) go through this so external accounts are
// supported uniformly without per-handler branching.
func dispatchChat(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	callback = restoreCallbackToolNames(payload, callback)
	if isCodexAccount(account) {
		return CallExternalCodex(account, payload, callback)
	}
	if isAgentRouterAccount(account) {
		return CallExternalAgentRouter(account, payload, callback)
	}
	if isExternalAccount(account) {
		return CallExternalOpenAI(account, payload, callback)
	}
	return CallKiroAPI(account, payload, callback)
}

// resolveExternalTestModel picks a concrete model ID for the provider-validation
// ping. Hard-coding "auto" fails on providers whose registry has no such alias
// (they answer HTTP 503 model_not_found), so the provider's own /v1/models list
// is consulted first and "auto" is kept only as a last-resort fallback for
// providers that expose no catalog at all.
func resolveExternalTestModel(account *config.Account) string {
	if account == nil {
		return "auto"
	}
	models, err := fetchExternalProviderModels(account)
	if err != nil || len(models) == 0 {
		return "auto"
	}
	// Prefer an explicit "auto" entry when the provider really advertises it.
	for _, m := range models {
		if strings.EqualFold(strings.TrimSpace(m.ModelId), "auto") {
			return "auto"
		}
	}
	for _, m := range models {
		if id := strings.TrimSpace(m.ModelId); id != "" {
			return id
		}
	}
	return "auto"
}

// testExternalProvider performs a minimal chat-completion round-trip against
// the external provider to validate the base_url + api_key pair. Returns the
// upstream latency on success, or an error describing the failure.
func testExternalProvider(account *config.Account) (time.Duration, error) {
	start := time.Now()
	if isAgentRouterAccount(account) {
		if _, err := CallAgentRouterTest(account); err != nil {
			return 0, err
		}
		return time.Since(start), nil
	}

	model := resolveExternalTestModel(account)
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "omniproxy-test"
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "ping",
		ModelID: model,
		Origin:  "AI_EDITOR",
	}
	payload.OriginalModel = model
	done := make(chan struct{})
	var callErr error
	cb := &KiroStreamCallback{
		OnError: func(err error) { callErr = err },
	}
	go func() {
		callErr = dispatchChat(account, payload, cb)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		return 0, fmt.Errorf("timeout")
	}
	if callErr != nil {
		return 0, callErr
	}
	return time.Since(start), nil
}

// ExternalProviderMe is the response shape returned by an external
// OpenAI-compatible provider's /api/me endpoint. Not all fields are populated
// by every provider; consumers must treat missing fields as zero/empty.
type ExternalProviderMe struct {
	CreditLimit      float64 `json:"creditLimit"`
	CreditsRemaining float64 `json:"creditsRemaining"`
	CreditsUsed      float64 `json:"creditsUsed"`
	RequestsCount    int64   `json:"requestsCount"`
	Status           string  `json:"status"`
	KeyMasked        string  `json:"keyMasked"`
	LastUsedAt       int64   `json:"lastUsedAt"`
	TokensUsed       int64   `json:"tokensUsed"`
	TokenLimit       int64   `json:"tokenLimit"`
	TokensRemaining  int64   `json:"tokensRemaining"`
}

// fetchExternalProviderCredits queries the external provider's /api/me
// endpoint and returns the credit/usage snapshot. The endpoint is optional:
// providers that do not implement /api/me return an error which the caller
// treats as "no credit info available" (non-fatal).
func fetchExternalProviderCredits(account *config.Account) (*ExternalProviderMe, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("no baseUrl")
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return nil, fmt.Errorf("no apiKey")
	}

	req, err := http.NewRequest("GET", baseURL+"/api/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client := GetRestClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		// /api/me is optional — many OpenAI-compatible providers only expose
		// /v1/* endpoints. Treat 404 as "credits API not supported" so the
		// refresh button doesn't surface a scary error.
		return nil, ErrExternalCreditsNotSupported
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(b))
	}

	var me ExternalProviderMe
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, err
	}
	return &me, nil
}
