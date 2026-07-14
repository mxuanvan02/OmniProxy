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
	"regexp"
	"strings"
	"superkiro/config"
	"superkiro/logger"
	"time"
)

// externalAuthMethod is the AuthMethod value marking an external OpenAI-compatible provider.
const externalAuthMethod = "external_openai"

// isExternalAccount reports whether the account routes to an external
// OpenAI-compatible provider instead of the native Kiro/AWS backend.
func isExternalAccount(account *config.Account) bool {
	return account != nil && account.AuthMethod == externalAuthMethod
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

	endpoint := baseURL + "/v1/chat/completions"
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
	// Kiro accounts advertise models with a "kr/" prefix (and SuperKiro uses
	// "superkiro/" internally for combo routing); external OpenAI-compatible
	// providers receive the bare model ID so their model registry can match it.
	modelID = stripInternalModelPrefix(modelID)
	// External providers (e.g. bddevlab) use dash-form model IDs
	// ("claude-opus-4-8") while SuperKiro's ParseModelAndThinking normalises
	// to dot-form ("claude-opus-4.8"). Revert to dash-form so the external
	// provider's model registry can match. Only applies to claude-* models;
	// other model families (gpt-*, o1-*, etc.) pass through unchanged.
	modelID = dotToDashClaudeVersion(modelID)
	// Resolve against the provider's model list: some providers use dated
	// snapshots (e.g. "claude-haiku-4-5-20251001") instead of short IDs.
	// Prefix-match the cached list to pick the closest available model.
	if account != nil {
		modelID = resolveExternalModelID(account, modelID)
	}

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

	// Tools — restore original (pre-sanitization) names for the external provider.
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(cur.UserInputMessageContext.Tools))
		for _, tw := range cur.UserInputMessageContext.Tools {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        restoreToolName(payload, tw.ToolSpecification.Name),
					"description": tw.ToolSpecification.Description,
					"parameters":  tw.ToolSpecification.InputSchema.JSON,
				},
			})
		}
		body["tools"] = tools
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
	}

	return body, nil
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

// dotToDashClaudeVersion reverts SuperKiro's dot-form normalisation
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

// stripInternalModelPrefix removes routing prefixes that SuperKiro / Kiro use
// internally but external OpenAI-compatible providers don't understand:
//   - "kr/"        — Kiro account model prefix (e.g. "kr/claude-sonnet-5")
//   - "superkiro/" — SuperKiro combo routing prefix
//
// The bare model ID is returned so the external provider's own model registry
// can match it (e.g. "claude-sonnet-5"). Unknown prefixes pass through unchanged.
func stripInternalModelPrefix(model string) string {
	for _, p := range []string{"kr/", "superkiro/"} {
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

// ==================== SSE parsing ====================

type externalToolAccum struct {
	ID        string
	Name      string
	Arguments string
}

// parseExternalOpenAISSE reads an OpenAI streaming chat-completion SSE stream
// and emits KiroStreamCallback events.
func parseExternalOpenAISSE(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	br := bufio.NewReaderSize(body, 16*1024)

	var inputTokens, outputTokens int
	toolAccums := map[int]*externalToolAccum{}
	var toolOrder []int

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
			if err == io.EOF {
				if strings.TrimSpace(line) != "" {
					if handled := processExternalSSELine(line, callback, toolAccums, &toolOrder, &inputTokens, &outputTokens); handled {
						// last line had content
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
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Debugf("[ExternalOpenAI] unmarshal chunk failed: %v (data=%s)", err, data)
			continue
		}

		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" && callback.OnText != nil {
				callback.OnText(ch.Delta.Content, false)
			}
			if ch.Delta.ReasoningContent != "" && callback.OnText != nil {
				callback.OnText(ch.Delta.ReasoningContent, true)
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := toolAccums[tc.Index]
				if !ok {
					acc = &externalToolAccum{}
					toolAccums[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
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
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
	}

	emitToolCalls()
	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

// processExternalSSELine handles a single non-empty SSE data line. Returns true
// if the line was a recognised data event (used by the EOF tail handler).
func processExternalSSELine(line string, callback *KiroStreamCallback, toolAccums map[int]*externalToolAccum, toolOrder *[]int, inputTokens, outputTokens *int) bool {
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		return true
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return false
	}
	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" && callback.OnText != nil {
			callback.OnText(ch.Delta.Content, false)
		}
		if ch.Delta.ReasoningContent != "" && callback.OnText != nil {
			callback.OnText(ch.Delta.ReasoningContent, true)
		}
	}
	return true
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
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("external json parse: %w", err)
	}

	for _, ch := range resp.Choices {
		if ch.Message.Content != "" && callback.OnText != nil {
			callback.OnText(ch.Message.Content, false)
		}
		if ch.Message.ReasoningContent != "" && callback.OnText != nil {
			callback.OnText(ch.Message.ReasoningContent, true)
		}
		for _, tc := range ch.Message.ToolCalls {
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

	req, err := http.NewRequest("GET", baseURL+"/v1/models", nil)
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
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by,omitempty"`
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
		out = append(out, ModelInfo{ModelId: m.ID, ModelName: m.ID})
	}
	return out, nil
}

// dispatchChat routes a chat request to the appropriate upstream based on the
// account's auth method. Native Kiro/AWS accounts use CallKiroAPI; external
// OpenAI-compatible providers use CallExternalOpenAI. All response handlers
// (Claude/OpenAI, stream/non-stream) go through this so external accounts are
// supported uniformly without per-handler branching.
func dispatchChat(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if isExternalAccount(account) {
		return CallExternalOpenAI(account, payload, callback)
	}
	return CallKiroAPI(account, payload, callback)
}

// testExternalProvider performs a minimal chat-completion round-trip against
// the external provider to validate the base_url + api_key pair. Returns the
// upstream latency on success, or an error describing the failure.
func testExternalProvider(account *config.Account) (time.Duration, error) {
	start := time.Now()
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "superkiro-test"
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "ping",
		ModelID: "auto",
		Origin:  "AI_EDITOR",
	}
	done := make(chan struct{})
	var callErr error
	cb := &KiroStreamCallback{
		OnError: func(err error) { callErr = err },
	}
	go func() {
		callErr = CallExternalOpenAI(account, payload, cb)
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
