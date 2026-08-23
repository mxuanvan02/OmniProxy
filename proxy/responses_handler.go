package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"omniproxy/pool"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponse(req.PreviousResponseID)
		if loadErr != nil {
			h.sendOpenAIError(w, 404, "invalid_request_error",
				fmt.Sprintf("previous_response_id not found: %v", loadErr))
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:    stripProviderPrefix(req.Model),
		Messages: finalMessages,
		Stream:   req.Stream,
		Tools:    req.Tools,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	// Check if model is a combo name — only on the top-level request, not
	// on sub-requests dispatched by the combo handler itself.
	if r.Context().Value(comboBypassKey) == nil {
		if comboName, comboModels, ok := resolveComboModels(openaiReq.Model); ok {
			h.handleComboRequest(w, r, comboName, comboModels, body, "responses")
			return
		}
	}

	thinkingCfg := config.GetThinkingConfig()
	originalModel := stripThinkingSuffix(req.Model, thinkingCfg.Suffix)
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)
	kiroPayload := OpenAIToKiro(openaiReq, thinking)
	kiroPayload.OriginalModel = originalModel

	// Forward reasoning.effort from the Responses API request (sent by
	// Codex CLI when model_reasoning_effort is set in config.toml). This
	// takes precedence over the Thinking-budget heuristic in OpenAIToKiro
	// because it is the explicit, user-configured value.
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		if kiroPayload.InferenceConfig == nil {
			kiroPayload.InferenceConfig = &InferenceConfig{}
		}
		kiroPayload.InferenceConfig.ReasoningEffort = req.Reasoning.Effort
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	respID := generateResponseID()

	if req.Stream {
		h.handleResponsesStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens,
			apiKeyID, respID, &req, storedInputCopy, storeResponse)
		return
	}

	h.handleResponsesNonStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens,
		apiKeyID, respID, &req, storedInputCopy, storeResponse)
}

func (h *Handler) handleResponsesNonStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	excluded := make(map[string]bool)
	var lastErr error
	// lastAccountID attributes the final failure to a concrete account in Usage.
	var lastAccountID string
	cacheKey := payloadCacheKey(payload)

	for attempt := 0; ; attempt++ {
		account := h.pool.GetNextForModelWithCacheKey(model, excluded, cacheKey)
		if account == nil {
			break
		}
		h.logCacheRouting("responses-nonstream", model, cacheKey, payload, account)
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		realCacheRead := 0
		realCacheCreate := 0
		attemptProduced := false
		resetAttempt := func() {
			content = ""
			reasoningContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			realCacheRead = 0
			realCacheCreate = 0
			attemptProduced = false
		}
		callback := &KiroStreamCallback{
			OnOutput:  func() { attemptProduced = true },
			HasOutput: func() bool { return attemptProduced },
			OnReset: func() {
				if isExternalAccount(account) {
					resetAttempt()
				}
			},
			OnText: func(text string, isThinking bool) {
				if !isThinking && text != "" {
					attemptProduced = true
				}
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				attemptProduced = true
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnCacheRead: func(cachedTokens int) {
				if cachedTokens > realCacheRead {
					realCacheRead = cachedTokens
				}
			},
			OnCacheCreate: func(cacheCreateTokens int) {
				if cacheCreateTokens > realCacheCreate {
					realCacheCreate = cacheCreateTokens
				}
			},
		}

		h.usageTracker.TrackActive(account.ID, endpointOpenAIResponses, model)
		err := dispatchChat(account, payload, callback)
		if err != nil {
			lastErr = err
			// Transient upstream errors (5xx, overload, timeout) are retried
			// in-place with backoff before rotating to a different account.
			if h.tryTransientRetry(account, payload, callback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto responsesNonStreamSuccess
			}
			if pool.IsContentBlockedError(err) {
				lastErr = err
				logger.Warnf("[ContentBlocked] %s: upstream refused payload for model %s — skipping account (err: %s)", account.Email, model, truncateForLog(err.Error()))
				excluded[account.ID] = true
				continue
			}
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
	responsesNonStreamSuccess:

		finalContent, _ := extractThinkingFromContent(content)
		if !thinking {
			reasoningContent = ""
		}

		// Input precedence: exact upstream count (OnComplete) > context-percentage
		// derivation > pre-request estimate.
		if inputTokens <= 0 {
			if realInputTokens > 0 {
				inputTokens = realInputTokens
			} else {
				inputTokens = estimatedInputTokens
			}
		}
		// Only estimate output when upstream did not report a real count.
		if outputTokens <= 0 {
			outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)
		}

		h.recordUsageWithCache(apiKeyID, account.ID, model, endpointOpenAIResponses, inputTokens, outputTokens, credits, cacheUsageTelemetry{
			CreateTokens: realCacheCreate,
			CachedTokens: realCacheRead,
		})
		h.pool.RecordSuccess(account.ID, model)
		if cacheKey != "" {
			h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
		}
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, realCacheRead, req)
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordError(apiKeyID, lastAccountID, model, endpointOpenAIResponses, lastErr.Error())
	h.sendOpenAIError(w, upstreamErrorStatus(lastErr), "server_error", lastErr.Error())
}

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens, cachedTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 1+len(toolUses))

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	usage := ResponsesUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}
	if cachedTokens > 0 {
		usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: cachedTokens}
	}

	return &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             "completed",
		Model:              model,
		Output:             output,
		Usage:              usage,
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
}

func (h *Handler) handleResponsesStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})

	excluded := make(map[string]bool)
	var lastErr error
	// lastAccountID attributes the final failure to a concrete account in Usage.
	var lastAccountID string
	responseStarted := false
	realCacheRead := 0
	realCacheCreate := 0
	cacheKey := payloadCacheKey(payload)

	for attempt := 0; ; attempt++ {
		account := h.pool.GetNextForModelWithCacheKey(model, excluded, cacheKey)
		if account == nil {
			break
		}
		h.logCacheRouting("responses-stream", model, cacheKey, payload, account)
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
		realCacheRead = 0
		realCacheCreate = 0

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		var (
			fullText        strings.Builder
			reasoningText   strings.Builder
			toolUses        []KiroToolUse
			inputTokens     int
			outputTokens    int
			credits         float64
			realInputTokens int
		)

		messageItemID := generateOutputItemID("msg")
		messageStarted := false
		outputIndex := 0
		contentIndex := 0

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
		}

		callback := &KiroStreamCallback{
			// Mark output at the adapter boundary, before any downstream
			// buffering or formatting. A failed stream must not be replayed after
			// the provider has emitted reasoning, text, or a tool call.
			OnOutput: func() { responseStarted = true },
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					reasoningText.WriteString(text)
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
			},
			OnToolUse: func(tu KiroToolUse) {
				if messageStarted {
					send("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       messageItemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullText.String(),
						},
					})
					send("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": outputIndex,
						"item": map[string]interface{}{
							"id":     messageItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []map[string]interface{}{{
								"type": "output_text",
								"text": fullText.String(),
							}},
						},
					})
					messageStarted = false
					outputIndex++
				}

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnCacheRead: func(cachedTokens int) {
				if cachedTokens > realCacheRead {
					realCacheRead = cachedTokens
				}
			},
			OnCacheCreate: func(cacheCreateTokens int) {
				if cacheCreateTokens > realCacheCreate {
					realCacheCreate = cacheCreateTokens
				}
			},
		}

		h.usageTracker.TrackActive(account.ID, endpointOpenAIResponses, model)
		// Codex accounts: wrap callback with downstream coalescing to cut
		// per-token json.Marshal + Flush syscalls ~50-100x. Only safe
		// pre-stream-start (coalescer flushes on terminal events so the
		// response.created / first delta ordering is preserved).
		effectiveCallback := callback
		if isCodexAccount(account) {
			effectiveCallback = newCodexCoalescer(callback)
		}
		err := dispatchChat(account, payload, effectiveCallback)
		var finalContent string
		var reasoning string
		if err != nil {
			// Transient upstream errors (5xx, overload, timeout) are retried
			// in-place with backoff before rotating. Only safe if we haven't
			// started streaming the response yet (otherwise we'd duplicate
			// the response.created event).
			if !responseStarted && h.tryTransientRetry(account, payload, effectiveCallback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto responsesStreamSuccess
			}
			if pool.IsContentBlockedError(err) {
				lastErr = err
				logger.Warnf("[ContentBlocked] %s: upstream refused payload for model %s — skipping account (err: %s)", account.Email, model, truncateForLog(err.Error()))
				excluded[account.ID] = true
				continue
			}
			if !responseStarted {
				lastErr = err
				h.usageTracker.RemoveActive(account.ID)
				excluded[account.ID] = true
				h.handleAccountFailure(account, err, model)
				continue
			}
			if effectiveCallback.OnError != nil {
				effectiveCallback.OnError(err)
			}
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": err.Error(),
					},
				},
			})
			h.recordFailure()
			return
		}

	responsesStreamSuccess:
		finalContent, _ = extractThinkingFromContent(fullText.String())
		reasoning = reasoningText.String()
		if !thinking {
			reasoning = ""
		}

		if messageStarted {
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": finalContent,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": finalContent,
					}},
				},
			})
		}

		// Input precedence: exact upstream count (OnComplete) > context-percentage
		// derivation > pre-request estimate. realInputTokens is the pct×window
		// fallback; prefer a real counted inputTokens when upstream provided one.
		if inputTokens <= 0 {
			if realInputTokens > 0 {
				inputTokens = realInputTokens
			} else {
				inputTokens = estimatedInputTokens
			}
		}
		// Only estimate output when upstream did not report a real count.
		if outputTokens <= 0 {
			outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)
		}

		h.recordUsageWithCache(apiKeyID, account.ID, model, endpointOpenAIResponses, inputTokens, outputTokens, credits, cacheUsageTelemetry{
			CreateTokens: realCacheCreate,
			CachedTokens: realCacheRead,
		})
		h.pool.RecordSuccess(account.ID, model)
		if cacheKey != "" {
			h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
		}
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, realCacheRead, req)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		send("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respObj,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	h.recordError(apiKeyID, lastAccountID, model, endpointOpenAIResponses, lastErr.Error())
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    "server_error",
				"message": lastErr.Error(),
			},
		},
	})
}
