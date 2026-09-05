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
	"context"
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

// Several OpenAI-compatible resale gateways (tabitoken.com, gorouter.app,
// api.justwoker.icu, ...) sit behind a Cloudflare bot-fight rule that answers
// Go's default "Go-http-client/2.0" User-Agent with an HTML 403 challenge page,
// on POST /v1/chat/completions only. Measured on 2026-09-06 against
// tabitoken.com: no User-Agent -> 403 HTML; a User-Agent alone -> 403; a
// User-Agent plus at least one x-stainless-* header -> 200. The rule therefore
// fingerprints "real OpenAI SDK client", so the adapter presents the same
// header set the official openai-python client sends. These values describe the
// caller, not the account, so they are constants rather than config.
const (
	externalOpenAIUserAgent           = "OpenAI/Python 1.109.1"
	externalOpenAIStainlessLang       = "python"
	externalOpenAIStainlessPackageVer = "1.109.1"
	externalOpenAIStainlessRuntime    = "CPython"
	externalOpenAIStainlessRuntimeVer = "3.12.7"
	externalOpenAIStainlessOS         = "MacOS"
	externalOpenAIStainlessArch       = "arm64"
)

// defaultExternalChatPath is the OpenAI chat-completions path used unless the
// account overrides it.
const defaultExternalChatPath = "/v1/chat/completions"

// externalChatPath returns the upstream chat path for an account: the
// account-level ChatPath override when set, otherwise the OpenAI default.
func externalChatPath(account *config.Account) string {
	if account == nil {
		return defaultExternalChatPath
	}
	if p := strings.TrimSpace(account.ChatPath); p != "" {
		return "/" + strings.TrimLeft(p, "/")
	}
	return defaultExternalChatPath
}

// setExternalOpenAIHeaders applies the OpenAI-SDK-shaped identity headers to an
// outbound request. accept selects the response dialect ("text/event-stream"
// for streaming chat, "application/json" for REST reads).
func setExternalOpenAIHeaders(req *http.Request, apiKey, accept string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", externalOpenAIUserAgent)
	req.Header.Set("x-stainless-lang", externalOpenAIStainlessLang)
	req.Header.Set("x-stainless-package-version", externalOpenAIStainlessPackageVer)
	req.Header.Set("x-stainless-runtime", externalOpenAIStainlessRuntime)
	req.Header.Set("x-stainless-runtime-version", externalOpenAIStainlessRuntimeVer)
	req.Header.Set("x-stainless-os", externalOpenAIStainlessOS)
	req.Header.Set("x-stainless-arch", externalOpenAIStainlessArch)
	req.Header.Set("x-stainless-retry-count", "0")
}

// ErrExternalCreditsNotSupported is returned by fetchExternalProviderCredits
// when none of the known billing dialects answer. Callers treat this as a
// non-fatal "no credit info available" condition rather than a hard error, so
// the refresh button stays green even for providers that only expose /v1/chat.
var ErrExternalCreditsNotSupported = fmt.Errorf("provider exposes no known credits endpoint")

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
func CallExternalOpenAI(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
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

	endpoint := openAICompatibleEndpoint(baseURL, externalChatPath(account))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("external call new request: %w", err)
	}
	setExternalOpenAIHeaders(req, apiKey, "text/event-stream")

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
	if cacheControlPassthroughEnabled(account) && len(msgs) > 0 {
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

// cacheControlPassthroughEnabled resolves whether cache_control breakpoints
// should be attached for one account. The per-account override wins when set;
// otherwise the global Settings switch applies.
//
// Rollout safety is the whole point of the override: the external pool spans
// many independent gateways, and enabling the global flag would bet every one
// of them at once on honouring an Anthropic-only field. Canarying a single
// account keeps a rejection blast radius of one.
func cacheControlPassthroughEnabled(account *config.Account) bool {
	if account != nil && account.CacheControlPassthrough != nil {
		return *account.CacheControlPassthrough
	}
	return config.GetCacheControlPassthrough()
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
	// Withhold whitespace-only text so a turn that never produces real content
	// stays retryable instead of reaching the client as a finished, empty answer.
	gate := newBlankOutputGate(callback)
	callback = gate.callback()
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
		// Metadata-only chunks such as {"delta":{"role":"assistant"}}
		// do not prove that generation is making progress. Keep the shorter
		// initial-data deadline until text, reasoning, or a tool call arrives;
		// otherwise one empty chunk can leave the request idle for the full
		// stream timeout.
		recordExternalSSEProgress(watchdog, result)
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
	// A stream can be well-formed and still carry nothing renderable (a lone
	// space with finish_reason "length"). Report it so the caller can rotate
	// accounts rather than closing the turn on an empty answer.
	if !gate.meaningful {
		return blankTurnError(stopReason)
	}
	if callback.OnStopReason != nil {
		callback.OnStopReason(stopReason)
	}
	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

func recordExternalSSEProgress(watchdog *sseIdleWatchdog, result externalSSELineResult) {
	if watchdog != nil && result.recognizedOutput {
		watchdog.DataReceived()
	}
}

// blankOutputGate withholds assistant text until the turn proves it carries
// real content. Some upstreams answer a long agentic turn with a single space
// and finish_reason "length": a technically well-formed stream that a client
// renders as a finished, empty turn. Forwarding that space immediately would
// mark the attempt as "already produced output", which costs the proxy its
// ability to rotate to another account. Holding whitespace back keeps the
// attempt retryable while preserving byte-exact output when content does
// arrive: the pending prefix is flushed in order ahead of the first real text.
type blankOutputGate struct {
	target     *KiroStreamCallback
	pending    []pendingText
	meaningful bool
	announced  bool
}

type pendingText struct {
	text       string
	isThinking bool
}

func newBlankOutputGate(target *KiroStreamCallback) *blankOutputGate {
	return &blankOutputGate{target: target}
}

// callback returns a view of the target with text and tool events gated.
// Every other callback is passed through untouched.
func (g *blankOutputGate) callback() *KiroStreamCallback {
	if g.target == nil {
		return &KiroStreamCallback{}
	}
	gated := *g.target
	gated.OnText = g.onText
	if g.target.OnToolUse != nil {
		gated.OnToolUse = func(toolUse KiroToolUse) {
			// A tool call is real work even with no text alongside it.
			// Tool calls are accumulated during the stream and emitted once the
			// terminal event arrives, so the parser's own OnOutput signal for
			// the tool delta was suppressed while the turn still looked blank.
			// Announce it here instead, or a tool-only turn would reach the
			// caller with no record that the upstream produced anything.
			g.meaningful = true
			g.announceOutput()
			g.flush()
			g.target.OnToolUse(toolUse)
		}
	}
	// OnOutput would otherwise announce whitespace as produced output.
	gated.OnOutput = func() {
		if g.meaningful {
			g.announceOutput()
		}
	}
	return &gated
}

// announceOutput forwards the "upstream produced output" signal at most once,
// so a turn carrying both text and tool calls does not report it twice.
func (g *blankOutputGate) announceOutput() {
	if g.announced {
		return
	}
	g.announced = true
	if g.target.OnOutput != nil {
		g.target.OnOutput()
	}
}

func (g *blankOutputGate) onText(text string, isThinking bool) {
	if text == "" {
		return
	}
	if !g.meaningful && strings.TrimSpace(text) == "" {
		g.pending = append(g.pending, pendingText{text: text, isThinking: isThinking})
		return
	}
	g.meaningful = true
	g.announceOutput()
	g.flush()
	if g.target.OnText != nil {
		g.target.OnText(text, isThinking)
	}
}

// flush releases withheld whitespace once the turn is known to be real.
func (g *blankOutputGate) flush() {
	if len(g.pending) == 0 {
		return
	}
	if g.target.OnText != nil {
		for _, p := range g.pending {
			g.target.OnText(p.text, p.isThinking)
		}
	}
	g.pending = nil
}

// blankTurnError reports a turn that terminated without any renderable
// assistant content. Returning an error lets the caller rotate accounts
// instead of handing the client a finished, empty answer.
//
// The message deliberately carries no digits: error classification elsewhere
// matches bare status tokens such as 401/403/503 anywhere in the string, so an
// embedded token count could be misread as an upstream status code and trigger
// an unrelated auth refresh or cooldown.
func blankTurnError(stopReason string) error {
	if stopReason == "max_tokens" {
		return fmt.Errorf("external upstream stopped at its output limit before producing any content (stop_reason=%s)", stopReason)
	}
	return fmt.Errorf("external upstream ended with blank assistant output (stop_reason=%s)", stopReason)
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
	// Same blank-turn protection as the SSE path: a provider that ignores
	// stream=true can still answer with whitespace only.
	gate := newBlankOutputGate(callback)
	callback = gate.callback()
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
	if !gate.meaningful {
		return blankTurnError(stopReason)
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
	setExternalOpenAIHeaders(req, apiKey, "application/json")

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

// imageCapabilityMetadata reports an image-output marker only when the provider
// actually claims image generation. Capability objects are keyed by capability
// name, so a key on its own proves nothing: flattening keys together with
// values made a provider that spells out {"image": false} look like a generator.
func imageCapabilityMetadata(value interface{}) []string {
	if capabilityClaimsImageOutput(value) {
		return []string{"image output"}
	}
	return nil
}

// capabilityClaimsImageOutput walks a capability tree. A bare string counts on
// its own because capabilities also arrive as lists such as
// ["image_generation"], while a keyed entry counts only when its value is
// enabled.
func capabilityClaimsImageOutput(value interface{}) bool {
	switch item := value.(type) {
	case string:
		return isImageOutputCapabilityName(item)
	case []interface{}:
		for _, child := range item {
			if capabilityClaimsImageOutput(child) {
				return true
			}
		}
	case map[string]interface{}:
		for key, child := range item {
			if isImageOutputCapabilityName(key) {
				if capabilityValueEnabled(child) {
					return true
				}
				continue
			}
			if capabilityClaimsImageOutput(child) {
				return true
			}
		}
	}
	return false
}

// isImageOutputCapabilityName is deliberately narrower than "mentions image":
// image_input and image_vision describe what a model accepts, not what it emits.
func isImageOutputCapabilityName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if !strings.Contains(name, "image") {
		return false
	}
	// "generat" covers generate, generation and generative: providers spell the
	// same capability all three ways.
	return name == "image" || strings.Contains(name, "output") || strings.Contains(name, "generat")
}

// capabilityValueEnabled resolves the value under a capability key. An explicit
// false, zero, empty string or empty container means unsupported; a nested
// object states its own support, as in {"supported": true}.
func capabilityValueEnabled(value interface{}) bool {
	switch item := value.(type) {
	case bool:
		return item
	case float64:
		return item != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "", "false", "0", "no", "none", "off", "unsupported", "disabled":
			return false
		}
		return true
	case []interface{}:
		for _, child := range item {
			if capabilityValueEnabled(child) {
				return true
			}
		}
		return false
	case map[string]interface{}:
		if supported, ok := item["supported"]; ok {
			return capabilityValueEnabled(supported)
		}
		for _, child := range item {
			if capabilityValueEnabled(child) {
				return true
			}
		}
		return false
	}
	return false
}

// dispatchChat routes a chat request to the appropriate upstream based on the
// account's auth method. Native Kiro/AWS accounts use CallKiroAPI; external
// OpenAI-compatible providers use CallExternalOpenAI. All response handlers
// (Claude/OpenAI, stream/non-stream) go through this so external accounts are
// supported uniformly without per-handler branching.
// ctx carries the client's request lifetime: when the caller disconnects the
// upstream call is cancelled instead of streaming (and billing) into a dead
// connection until the idle watchdog fires.
func dispatchChat(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	callback = restoreCallbackToolNames(payload, callback)
	if isCodexAccount(account) {
		return CallExternalCodex(ctx, account, payload, callback)
	}
	if isAntigravityAccount(account) {
		return CallExternalAntigravity(ctx, account, payload, callback)
	}
	if isAgentRouterAccount(account) {
		return CallExternalAgentRouter(ctx, account, payload, callback)
	}
	if isExternalAccount(account) {
		return CallExternalOpenAI(ctx, account, payload, callback)
	}
	return CallKiroAPI(ctx, account, payload, callback)
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
	// The timeout now cancels the upstream call rather than only abandoning the
	// goroutine waiting on it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	safeGo("testExternalProvider/dispatch", func() {
		defer close(done)
		callErr = dispatchChat(ctx, account, payload, cb)
	})
	select {
	case <-done:
	case <-ctx.Done():
		return 0, fmt.Errorf("timeout")
	}
	if callErr != nil {
		return 0, callErr
	}
	return time.Since(start), nil
}

// ExternalProviderMe is the normalised credit/usage snapshot for an external
// OpenAI-compatible provider. Every billing dialect in fetchExternalProviderCredits
// is mapped onto this shape; not all fields are populated by every dialect, so
// consumers must treat missing fields as zero/empty. Monetary fields are USD.
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

// unlimitedCreditSentinel is the threshold above which a reported quota means
// "unmetered". one-api/new-api forks answer hard_limit_usd = 100000000 for keys
// with no quota cap; rendering that as a limit would make every usage bar read
// 0%. Real prepaid keys are orders of magnitude below this.
const unlimitedCreditSentinel = 1e7

// fetchExternalProviderCredits returns the provider's credit snapshot, trying
// each known billing dialect in turn. Every dialect is optional: when none
// answers, the caller gets ErrExternalCreditsNotSupported and treats it as
// "no credit info available" rather than a failure.
func fetchExternalProviderCredits(account *config.Account) (*ExternalProviderMe, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("no baseUrl")
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return nil, fmt.Errorf("no apiKey")
	}
	dialects := []func(string, *config.Account) (*ExternalProviderMe, error){
		fetchCreditsAPIMe,
		fetchCreditsV1Me,
		fetchCreditsDashboardBilling,
	}
	// A real error from one dialect must not hide a later dialect that works,
	// so keep the first one and report it only if every attempt fails.
	var firstErr error
	for _, fetch := range dialects {
		me, err := fetch(baseURL, account)
		if err == nil {
			return me, nil
		}
		if err != ErrExternalCreditsNotSupported && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, ErrExternalCreditsNotSupported
}

// normalizeCreditSnapshot fills in whichever of limit/remaining/used the dialect
// left implicit and collapses an "unmetered" sentinel quota to zero, which the
// admin UI renders as "no limit" instead of a permanently-0% usage bar.
func normalizeCreditSnapshot(me *ExternalProviderMe) {
	// The sentinel can arrive in either field depending on the dialect: as a
	// ceiling (spendLimit) or as a remaining balance (one-api hard_limit_usd).
	// Zero both, or the derivation below would turn it into a bogus limit.
	if me.CreditsRemaining >= unlimitedCreditSentinel {
		me.CreditsRemaining = 0
		me.CreditLimit = 0
	}
	if me.CreditLimit >= unlimitedCreditSentinel {
		me.CreditLimit = 0
	}
	if me.CreditLimit == 0 && me.CreditsRemaining > 0 {
		me.CreditLimit = me.CreditsRemaining + me.CreditsUsed
	}
	if me.CreditsRemaining == 0 && me.CreditLimit > me.CreditsUsed {
		me.CreditsRemaining = me.CreditLimit - me.CreditsUsed
	}
}

// getProviderJSON performs an authenticated GET and decodes a JSON body. A
// 404/405/501, or a non-JSON body (providers that serve their SPA for unknown
// paths), means this dialect is absent rather than broken.
func getProviderJSON(account *config.Account, url string, out interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	setExternalOpenAIHeaders(req, strings.TrimSpace(account.AccessToken), "application/json")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404, 405, 501:
		return ErrExternalCreditsNotSupported
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(b))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "json") {
		return ErrExternalCreditsNotSupported
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// providerRootURL strips a trailing "/v1" so root-level endpoints resolve for
// accounts whose baseUrl already carries the version segment.
func providerRootURL(baseURL string) string {
	return strings.TrimSuffix(baseURL, "/v1")
}

// fetchCreditsAPIMe reads the snapshot some providers expose at /api/me already
// shaped like ExternalProviderMe.
func fetchCreditsAPIMe(baseURL string, account *config.Account) (*ExternalProviderMe, error) {
	var me ExternalProviderMe
	if err := getProviderJSON(account, providerRootURL(baseURL)+"/api/me", &me); err != nil {
		return nil, err
	}
	// Decoded, but nothing recognisable — some other dialect's payload.
	if me.CreditLimit == 0 && me.CreditsRemaining == 0 && me.CreditsUsed == 0 && me.RequestsCount == 0 {
		return nil, ErrExternalCreditsNotSupported
	}
	normalizeCreditSnapshot(&me)
	return &me, nil
}

// fetchCreditsV1Me reads the /v1/me dialect: a per-key balance plus lifetime
// spend and per-model token counts.
func fetchCreditsV1Me(baseURL string, account *config.Account) (*ExternalProviderMe, error) {
	var raw struct {
		Name          string   `json:"name"`
		Balance       *float64 `json:"balance"`
		SpendLimit    *float64 `json:"spendLimit"`
		TotalSpent    float64  `json:"totalSpent"`
		TotalRequests int64    `json:"totalRequests"`
		ModelUsage    map[string]struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"modelUsage"`
	}
	if err := getProviderJSON(account, openAICompatibleEndpoint(baseURL, "/v1/me"), &raw); err != nil {
		return nil, err
	}
	if raw.Balance == nil && raw.TotalSpent == 0 && raw.TotalRequests == 0 {
		return nil, ErrExternalCreditsNotSupported
	}
	me := &ExternalProviderMe{
		CreditsUsed:   raw.TotalSpent,
		RequestsCount: raw.TotalRequests,
		KeyMasked:     raw.Name,
	}
	if raw.Balance != nil {
		me.CreditsRemaining = *raw.Balance
	}
	if raw.SpendLimit != nil {
		me.CreditLimit = *raw.SpendLimit
	}
	for _, u := range raw.ModelUsage {
		me.TokensUsed += u.InputTokens + u.OutputTokens
	}
	normalizeCreditSnapshot(me)
	return me, nil
}

// fetchCreditsDashboardBilling reads the one-api/new-api dialect. Note that
// hard_limit_usd there is the key's REMAINING quota (remain_quota/500000), not a
// ceiling, and /v1/dashboard/billing/usage reports consumed quota as USD×100
// (cents). The total limit is therefore derived as remaining + used.
func fetchCreditsDashboardBilling(baseURL string, account *config.Account) (*ExternalProviderMe, error) {
	var sub struct {
		HardLimitUSD float64 `json:"hard_limit_usd"`
		SoftLimitUSD float64 `json:"soft_limit_usd"`
	}
	if err := getProviderJSON(account, openAICompatibleEndpoint(baseURL, "/v1/dashboard/billing/subscription"), &sub); err != nil {
		return nil, err
	}
	me := &ExternalProviderMe{CreditsRemaining: sub.HardLimitUSD}
	if me.CreditsRemaining == 0 {
		me.CreditsRemaining = sub.SoftLimitUSD
	}
	var usage struct {
		TotalUsage float64 `json:"total_usage"`
	}
	if err := getProviderJSON(account, openAICompatibleEndpoint(baseURL, "/v1/dashboard/billing/usage"), &usage); err == nil {
		me.CreditsUsed = usage.TotalUsage / 100.0
	} else if err != ErrExternalCreditsNotSupported {
		return nil, err
	}
	normalizeCreditSnapshot(me)
	return me, nil
}
