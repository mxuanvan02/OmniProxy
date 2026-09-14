package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"sync"
	"time"
)

// comboBypassKey is used as a context value to indicate that the request
// is a sub-request of the combo handler (for the "responses" format),
// preventing infinite recursion in handleOpenAIResponses.
type contextKey string

const comboBypassKey contextKey = "combo_bypass"

// comboForceFallbackKey prevents a ranked adaptive chain from being rotated by
// the global combo strategy. omni-auto has already ordered the models.
const comboForceFallbackKey contextKey = "combo_force_fallback"

// comboRotationEntry tracks round-robin state for a single combo.
type comboRotationEntry struct {
	mu          sync.Mutex
	currentIdx  int
	stickyCount int
}

var comboRotationMap sync.Map // key: comboName -> *comboRotationEntry

// resolveComboModels returns (comboName, models, true) if modelStr is a known combo name.
// Returns ("", nil, false) if modelStr contains "/" (provider/model) or is not found.
func resolveComboModels(modelStr string) (string, []string, bool) {
	if strings.Contains(modelStr, "/") {
		return "", nil, false
	}
	entry := config.GetComboByName(modelStr)
	if entry == nil || len(entry.Models) < 1 {
		return "", nil, false
	}
	return entry.Name, entry.Models, true
}

// getRotatedModels returns the model list in execution order.
// For "fallback" strategy the order is unchanged.
// For "round-robin" strategy the list is rotated so a different model starts each turn.
func getRotatedModels(models []string, comboName, strategy string, stickyLimit int) []string {
	if strategy != "round-robin" || len(models) == 0 {
		return models
	}
	raw, _ := comboRotationMap.LoadOrStore(comboName, &comboRotationEntry{})
	entry := raw.(*comboRotationEntry)
	entry.mu.Lock()
	idx := entry.currentIdx
	entry.stickyCount++
	if entry.stickyCount >= stickyLimit {
		entry.stickyCount = 0
		entry.currentIdx = (entry.currentIdx + 1) % len(models)
	}
	entry.mu.Unlock()

	rotated := make([]string, len(models))
	for i := range models {
		rotated[i] = models[(idx+i)%len(models)]
	}
	return rotated
}

// isComboFallbackEligible reports whether an error should trigger fallback to the next model.
// Mirrors 9router's checkFallbackError logic.
func isComboFallbackEligible(status int, errMsg string) bool {
	lower := strings.ToLower(errMsg)
	switch {
	case status == 401 || status == 403:
		return false
	case strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "token invalid"):
		return false
	case status == 429 || strings.Contains(lower, "quota") || strings.Contains(lower, "rate limit"):
		return true
	case status == 402 || strings.Contains(lower, "overage"):
		return true
	case status == 502 || status == 503 || status == 504:
		return true
	default:
		return true
	}
}

// bufferingResponseWriter captures the response before committing it to the real writer.
type bufferingResponseWriter struct {
	recorder *httptest.ResponseRecorder
}

func newBufferingResponseWriter() *bufferingResponseWriter {
	return &bufferingResponseWriter{recorder: httptest.NewRecorder()}
}

func (b *bufferingResponseWriter) Header() http.Header {
	return b.recorder.Header()
}

func (b *bufferingResponseWriter) Write(p []byte) (int, error) {
	return b.recorder.Write(p)
}

func (b *bufferingResponseWriter) WriteHeader(code int) {
	b.recorder.WriteHeader(code)
}

func (b *bufferingResponseWriter) Flush() {
	// no-op: buffered — we flush to real writer only on success
}

func (b *bufferingResponseWriter) status() int       { return b.recorder.Code }
func (b *bufferingResponseWriter) bodyBytes() []byte { return b.recorder.Body.Bytes() }
func (b *bufferingResponseWriter) committed() bool   { return false }

// flushTo copies the buffered response to the real ResponseWriter.
func (b *bufferingResponseWriter) flushTo(w http.ResponseWriter) {
	for k, vs := range b.recorder.Header() {
		w.Header()[k] = append([]string(nil), vs...)
	}
	w.WriteHeader(b.recorder.Code)
	_, _ = w.Write(b.recorder.Body.Bytes())
}

// streamingPreludeWriter buffers only protocol setup events. Once the first
// meaningful SSE event appears it commits the prelude and becomes a direct
// pass-through. A failure before that point can still fall back to another
// model; after output is visible, replay is forbidden.
type streamingPreludeWriter struct {
	dst       http.ResponseWriter
	recorder  *httptest.ResponseRecorder
	didCommit bool
}

const maxStreamingPreludeBytes = 64 << 10

func newStreamingPreludeWriter(dst http.ResponseWriter) *streamingPreludeWriter {
	return &streamingPreludeWriter{dst: dst, recorder: httptest.NewRecorder()}
}

func (s *streamingPreludeWriter) Header() http.Header {
	if s.didCommit {
		return s.dst.Header()
	}
	return s.recorder.Header()
}

func (s *streamingPreludeWriter) WriteHeader(code int) {
	if s.didCommit {
		s.dst.WriteHeader(code)
		return
	}
	s.recorder.WriteHeader(code)
}

func (s *streamingPreludeWriter) Write(p []byte) (int, error) {
	if s.didCommit {
		return s.dst.Write(p)
	}
	n, err := s.recorder.Write(p)
	if err != nil {
		return n, err
	}
	if streamPreludeShouldCommit(s.recorder.Body.Bytes()) || s.recorder.Body.Len() >= maxStreamingPreludeBytes {
		if err := s.commit(); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *streamingPreludeWriter) Flush() {
	if s.didCommit {
		if flusher, ok := s.dst.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (s *streamingPreludeWriter) commit() error {
	if s.didCommit {
		return nil
	}
	for k, vs := range s.recorder.Header() {
		s.dst.Header()[k] = append([]string(nil), vs...)
	}
	code := s.recorder.Code
	if code == 0 {
		code = http.StatusOK
	}
	s.dst.WriteHeader(code)
	s.didCommit = true
	if s.recorder.Body.Len() == 0 {
		return nil
	}
	_, err := s.dst.Write(s.recorder.Body.Bytes())
	s.recorder.Body.Reset()
	return err
}

func (s *streamingPreludeWriter) status() int       { return s.recorder.Code }
func (s *streamingPreludeWriter) bodyBytes() []byte { return s.recorder.Body.Bytes() }
func (s *streamingPreludeWriter) committed() bool   { return s.didCommit }
func (s *streamingPreludeWriter) flushTo(http.ResponseWriter) {
	_ = s.commit()
}

// streamPreludeShouldCommit detects generated output or a clean terminal event.
// Setup-only and empty-delta events remain buffered so an error before useful
// output can still switch models. Error events never trigger commitment.
func streamPreludeShouldCommit(data []byte) bool {
	meaningful := [][]byte{
		[]byte(`"type":"tool_use"`),
		[]byte(`"tool_calls":[`),
		[]byte("data: [DONE]"),
		[]byte(`"type":"message_stop"`),
		[]byte(`"type":"response.completed"`),
	}
	errors := [][]byte{[]byte("event: error"), []byte(`"type":"error"`), []byte(`"error":{`)}
	firstMeaningful, firstError := -1, -1
	for _, marker := range meaningful {
		if idx := bytes.Index(data, marker); idx >= 0 && (firstMeaningful < 0 || idx < firstMeaningful) {
			firstMeaningful = idx
		}
	}
	for _, marker := range errors {
		if idx := bytes.Index(data, marker); idx >= 0 && (firstError < 0 || idx < firstError) {
			firstError = idx
		}
	}
	for _, field := range []string{"text", "thinking", "partial_json", "content", "reasoning_content", "delta"} {
		if idx := firstNonEmptyJSONStringField(data, field); idx >= 0 && (firstMeaningful < 0 || idx < firstMeaningful) {
			firstMeaningful = idx
		}
	}
	return firstMeaningful >= 0 && (firstError < 0 || firstMeaningful < firstError)
}

// firstNonEmptyJSONStringField returns the byte offset of the first JSON string
// field with a non-empty value. It understands escapes and tolerates whitespace
// after the colon without parsing unrelated SSE lines as one JSON document.
func firstNonEmptyJSONStringField(data []byte, field string) int {
	marker := []byte(`"` + field + `"`)
	for offset := 0; offset < len(data); {
		rel := bytes.Index(data[offset:], marker)
		if rel < 0 {
			return -1
		}
		start := offset + rel
		i := start + len(marker)
		for i < len(data) && (data[i] == ' ' || data[i] == '	' || data[i] == '\r' || data[i] == '\n') {
			i++
		}
		if i >= len(data) || data[i] != ':' {
			offset = start + len(marker)
			continue
		}
		i++
		for i < len(data) && (data[i] == ' ' || data[i] == '	') {
			i++
		}
		if i >= len(data) || data[i] != '"' {
			offset = start + len(marker)
			continue
		}
		valueStart := i
		i++
		escaped := false
		for i < len(data) {
			if escaped {
				escaped = false
				i++
				continue
			}
			if data[i] == '\\' {
				escaped = true
				i++
				continue
			}
			if data[i] == '"' {
				var value string
				if json.Unmarshal(data[valueStart:i+1], &value) == nil && value != "" {
					return start
				}
				break
			}
			i++
		}
		offset = start + len(marker)
	}
	return -1
}

type comboAttemptWriter interface {
	http.ResponseWriter
	http.Flusher
	status() int
	bodyBytes() []byte
	committed() bool
	flushTo(http.ResponseWriter)
}

// handleComboRequest is the combo execution engine.
// It tries each model in the combo chain until one succeeds or all fail.
//
// format must be "claude" or "openai".
// originalBody is the raw JSON request body (used to rebuild per-model requests).
func (h *Handler) handleComboRequest(
	w http.ResponseWriter,
	r *http.Request,
	comboName string,
	models []string,
	originalBody []byte,
	format string,
) {
	strategy := config.GetComboStrategy()
	forcedFallback, _ := r.Context().Value(comboForceFallbackKey).(bool)
	if forcedFallback {
		strategy = "fallback"
	}
	// Per-combo strategy override.
	if entry := config.GetComboByName(comboName); !forcedFallback && entry != nil && entry.Strategy != "" {
		strategy = entry.Strategy
	}
	stickyLimit := config.GetComboStickyRoundRobinLimit()
	rotated := getRotatedModels(models, comboName, strategy, stickyLimit)
	isStream := isComboRequestStreaming(originalBody)

	var lastStatus int
	var lastErrMsg string

	logger.Infof("[COMBO] %s starting — strategy=%s models=%d", comboName, strategy, len(rotated))

	for i, modelStr := range rotated {
		logger.Infof("[COMBO] %s attempt %d/%d model=%s", comboName, i+1, len(rotated), modelStr)

		// Resolve the model name through ParseModelAndThinking so the pool
		// lookup key matches exactly what single-model requests produce.
		// This normalizes dash/dot formats and strips any thinking suffix.
		thinkingCfg := config.GetThinkingConfig()
		resolvedModel, _ := ParseModelAndThinking(modelStr, thinkingCfg.Suffix)
		if resolvedModel != modelStr {
			logger.Debugf("[COMBO] %s model=%s resolved to %s", comboName, modelStr, resolvedModel)
		}

		// DIAG: log pool model lists for this model to check if any account supports it
		poolModelCount := h.pool.CountAccountsForModel(resolvedModel)
		totalAccounts := h.pool.Count()
		logger.Warnf("[COMBO] %s model=%s pool_check total_accounts=%d supporting_model=%d",
			comboName, modelStr, totalAccounts, poolModelCount)
		if totalAccounts == 0 {
			logger.Warnf("[COMBO] %s model=%s POOL IS EMPTY — no enabled/non-quota-blocked accounts",
				comboName, modelStr)
		}
		if poolModelCount == 0 && totalAccounts > 0 {
			logger.Warnf("[COMBO] %s model=%s NO ACCOUNT supports this model — model name may not match Kiro API output",
				comboName, modelStr)
		}

		// Patch the model name in the request body for this attempt.
		patchedBody, err := patchModelInBody(originalBody, modelStr, format)
		if err != nil {
			logger.Warnf("[COMBO] %s model=%s body patch failed: %v", comboName, modelStr, err)
			lastErrMsg = err.Error()
			lastStatus = 500
			continue
		}

		// Build a fresh *http.Request with the patched body.
		newReq, err := rebuildRequest(r, patchedBody)
		if err != nil {
			logger.Warnf("[COMBO] %s model=%s rebuild request failed: %v", comboName, modelStr, err)
			lastErrMsg = err.Error()
			lastStatus = 500
			continue
		}

		var attemptWriter comboAttemptWriter
		if isStream {
			attemptWriter = newStreamingPreludeWriter(w)
		} else {
			attemptWriter = newBufferingResponseWriter()
		}

		// Mark this as a combo sub-request so the dispatched handler skips
		// combo resolution (prevents infinite recursion when a combo model
		// shares the combo name).
		ctx := context.WithValue(newReq.Context(), comboBypassKey, true)
		newReq = newReq.WithContext(ctx)

		switch format {
		case "claude":
			h.handleClaudeMessages(attemptWriter, newReq)
		case "openai":
			h.handleOpenAIChat(attemptWriter, newReq)
		case "responses":
			h.handleOpenAIResponses(attemptWriter, newReq)
		}

		// Once meaningful stream output has escaped, retrying another model would
		// replay the prefix. The dispatched handler already emitted any terminal
		// error event, so the combo layer must return without writing again.
		if attemptWriter.committed() {
			return
		}

		body := attemptWriter.bodyBytes()
		status := attemptWriter.status()
		if status == 0 {
			status = 500
		}

		// Before commitment an SSE error is still eligible for model fallback.
		if status >= 200 && status < 300 && hasSSEErrorEvent(body) {
			logger.Warnf("[COMBO] %s model=%s SSE stream contained error before output", comboName, modelStr)
			status = 500
		}

		if status >= 200 && status < 300 {
			logger.Infof("[COMBO] %s model=%s succeeded status=%d", comboName, modelStr, status)
			attemptWriter.flushTo(w)
			return
		}

		// Extract error message from the bounded, uncommitted prelude/body.
		errMsg := extractErrorMessage(body)
		if errMsg == "" {
			errMsg = fmt.Sprintf("HTTP %d", status)
		}

		logger.Warnf("[COMBO] %s model=%s failed status=%d error=%s", comboName, modelStr, status, truncateStr(errMsg, 200))

		if lastStatus == 0 {
			lastStatus = status
		}
		lastErrMsg = errMsg

		if !isComboFallbackEligible(status, errMsg) {
			logger.Warnf("[COMBO] %s model=%s error not fallback-eligible, aborting chain", comboName, modelStr)
			attemptWriter.flushTo(w)
			return
		}

		// Brief wait for transient upstream errors before trying the next model.
		if (status == 502 || status == 503 || status == 504) && i < len(rotated)-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}

	// All models exhausted.
	if lastStatus == 0 {
		lastStatus = 503
	}
	logger.Warnf("[COMBO] %s all models failed — lastStatus=%d lastError=%s", comboName, lastStatus, truncateStr(lastErrMsg, 200))

	// Streaming clients expect a terminal SSE error when every model failed
	// before any candidate produced meaningful output.
	if isStream {
		flusher, ok := w.(http.Flusher)
		if ok {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(200)
			if format == "claude" {
				h.sendClaudeSSEError(w, flusher, "api_error", "All combo models failed: "+lastErrMsg)
			} else {
				h.sendOpenAISSEError(w, flusher, "server_error", "All combo models failed: "+lastErrMsg)
			}
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(lastStatus)
	errResp := map[string]interface{}{
		"error": map[string]string{
			"type":    "combo_exhausted",
			"message": "All combo models failed: " + lastErrMsg,
		},
	}
	json.NewEncoder(w).Encode(errResp) //nolint:errcheck
}

// isComboRequestStreaming checks if the original request body has stream:true.
func isComboRequestStreaming(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// patchModelInBody replaces the "model" field in the JSON request body.
func patchModelInBody(body []byte, modelStr, format string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	modelJSON, err := json.Marshal(modelStr)
	if err != nil {
		return nil, err
	}
	obj["model"] = modelJSON
	return json.Marshal(obj)
}

// rebuildRequest creates a new *http.Request with a fresh body from patchedBody.
func rebuildRequest(r *http.Request, patchedBody []byte) (*http.Request, error) {
	newReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), bytes.NewReader(patchedBody))
	if err != nil {
		return nil, err
	}
	// Copy headers.
	newReq.Header = r.Header.Clone()
	newReq.ContentLength = int64(len(patchedBody))
	return newReq, nil
}

// extractErrorMessage pulls a human-readable error string from a JSON response body.
func extractErrorMessage(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}
	// If the body is an SSE stream (starts with "event:" or "data:"),
	// extract the JSON payload from the last data: line and parse that.
	if bytes.HasPrefix(body, []byte("event:")) || bytes.HasPrefix(body, []byte("data:")) {
		return extractSSEErrorMessage(body)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return string(body)
	}
	// Claude style: {"type":"error","error":{"message":"..."}}
	if errRaw, ok := obj["error"]; ok {
		var errObj map[string]json.RawMessage
		if err := json.Unmarshal(errRaw, &errObj); err == nil {
			if msgRaw, ok := errObj["message"]; ok {
				var msg string
				if json.Unmarshal(msgRaw, &msg) == nil {
					return msg
				}
			}
		}
		// error might be a plain string
		var errStr string
		if json.Unmarshal(errRaw, &errStr) == nil {
			return errStr
		}
	}
	// Try top-level "message".
	if msgRaw, ok := obj["message"]; ok {
		var msg string
		if json.Unmarshal(msgRaw, &msg) == nil {
			return msg
		}
	}
	return string(body)
}

// truncateStr shortens s to at most n runes.
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// hasSSEErrorEvent checks whether the buffered response body contains an SSE
// error event. This detects mid-stream failures where the HTTP status was set
// to 200 by the initial SSE frames but the stream subsequently failed.
func hasSSEErrorEvent(body []byte) bool {
	// Claude SSE: event: error\ndata: {...}
	if bytes.Contains(body, []byte("event: error\n")) {
		return true
	}
	// OpenAI SSE: data: {"error":{"type":"api_error",...}}
	// Look for data: {"error": after the last valid chunk.
	return bytes.Contains(body, []byte(`data: {"error":`))
}

// extractSSEErrorMessage parses an SSE error body and returns the error message.
// Handles both Claude format (event: error\ndata: {"type":"error","error":...})
// and OpenAI format (data: {"error":{"message":"..."}}).
func extractSSEErrorMessage(body []byte) string {
	// Find the last "data:" line containing an error payload.
	lines := bytes.Split(body, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		jsonData := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(jsonData, []byte("[DONE]")) {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(jsonData, &obj) != nil {
			continue
		}
		// Claude: {"type":"error","error":{"message":"..."}}
		if errRaw, ok := obj["error"]; ok {
			var errObj map[string]json.RawMessage
			if json.Unmarshal(errRaw, &errObj) == nil {
				if msgRaw, ok := errObj["message"]; ok {
					var msg string
					if json.Unmarshal(msgRaw, &msg) == nil {
						return msg
					}
				}
			}
		}
		// OpenAI: {"error":{"message":"..."}}
		if msgRaw, ok := obj["message"]; ok {
			var msg string
			if json.Unmarshal(msgRaw, &msg) == nil {
				return msg
			}
		}
	}
	// Fallback: return trimmed body
	return truncateStr(string(body), 200)
}
