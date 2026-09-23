// Package proxy — Gemini-native inbound endpoints for Antigravity IDE endpoint override.
//
// When the IDE's language_server is configured with -cloud_code_endpoint pointing at
// OmniProxy, it calls these paths directly (plain HTTP, no TLS). This module handles
// the four v1internal:* actions and translates chat requests to OpenAI format for the pool.
package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"omniproxy/logger"
)

// antigravityInboundActions lists the paths the language_server calls.
const (
	agiLoadAction   = "/v1internal:loadCodeAssist"
	agiOnboardAction = "/v1internal:onboardUser"
	agiModelsAction  = "/v1internal:fetchAvailableModels"
	agiStreamPrefix  = "/v1internal:streamGenerateContent"
)

// isAntigravityInbound reports whether the request targets a Gemini-native inbound path.
func isAntigravityInbound(path string) bool {
	return path == agiLoadAction ||
		path == agiOnboardAction ||
		path == agiModelsAction ||
		strings.HasPrefix(path, agiStreamPrefix)
}

// handleAntigravityInbound routes Gemini-native requests from the IDE's language_server.
// These endpoints are unauthenticated: the LS does not send API keys, and the endpoint
// is only reachable on loopback (127.0.0.1:8080). If OmniProxy ever binds 0.0.0.0,
// these must be gated by auth or IP check.
func (h *Handler) handleAntigravityInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		logger.Warnf("[AGI-Inbound] read body: %v", err)
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}

	// Log every request for discovery during development. Content is logged at Debug
	// level so production logs stay clean; enable via log_level=debug in config.
	logger.Infof("[AGI-Inbound] %s %s (%d bytes)", r.Method, r.URL.Path, len(body))
	logger.Debugf("[AGI-Inbound] body: %s", truncateForLog(string(body)))

	switch {
	case r.URL.Path == agiLoadAction:
		h.agiLoadCodeAssist(w, body)
	case r.URL.Path == agiOnboardAction:
		h.agiOnboardUser(w, body)
	case r.URL.Path == agiModelsAction:
		h.agiFetchModels(w, body)
	case strings.HasPrefix(r.URL.Path, agiStreamPrefix):
		h.agiStreamChat(w, r, body)
	default:
		http.Error(w, `{"error":"unknown action"}`, http.StatusNotFound)
	}
}

// agiLoadCodeAssist responds to the IDE's project/tier query. Returns a synthetic
// response that satisfies the LS without calling Google. The IDE uses this to
// determine which project to bill against; we return a placeholder since billing
// goes through OmniProxy's pool accounts instead.
func (h *Handler) agiLoadCodeAssist(w http.ResponseWriter, body []byte) {
	// Parse metadata to extract any project hint the LS sends.
	var req struct {
		Metadata map[string]string `json:"metadata"`
	}
	_ = json.Unmarshal(body, &req)

	projectID := "omniproxy-pool"
	tier := "standard-tier"

	resp := map[string]interface{}{
		"cloudaicompanionProject": projectID,
		"tier":                    tier,
		"success":                 true,
	}
	writeJSON(w, http.StatusOK, resp)
	logger.Infof("[AGI-Inbound] loadCodeAssist → project=%s tier=%s", projectID, tier)
}

// agiOnboardUser responds to the IDE's project provisioning request. In normal
// operation Google creates a managed project; here we return a pre-configured one.
func (h *Handler) agiOnboardUser(w http.ResponseWriter, body []byte) {
	resp := map[string]interface{}{
		"name":    "operations/omniproxy-onboard",
		"done":    true,
		"response": map[string]interface{}{
			"cloudaicompanionProject": "omniproxy-pool",
			"tier":                    "standard-tier",
		},
	}
	writeJSON(w, http.StatusOK, resp)
	logger.Infof("[AGI-Inbound] onboardUser → done")
}

// agiFetchModels returns the model catalog. Aggregates models known to the pool
// so the IDE sees what's actually available through OmniProxy.
func (h *Handler) agiFetchModels(w http.ResponseWriter, body []byte) {
	// TODO(phase-2): aggregate real model list from pool. For now return a
	// minimal set so the LS doesn't error out during discovery.
	models := []map[string]interface{}{
		{"id": "gemini-3-flash-agent", "displayName": "Gemini 3 Flash Agent"},
		{"id": "gemini-3.1-pro-low", "displayName": "Gemini 3.1 Pro Low"},
		{"id": "claude-opus-4-6-thinking", "displayName": "Claude Opus 4.6 Thinking"},
		{"id": "claude-sonnet-4-6", "displayName": "Claude Sonnet 4.6"},
	}
	resp := map[string]interface{}{
		"models": models,
	}
	writeJSON(w, http.StatusOK, resp)
	logger.Infof("[AGI-Inbound] fetchAvailableModels → %d models", len(models))
}

// agiStreamChat handles :streamGenerateContent?alt=sse — the main chat path.
// Translates Gemini-native request → OpenAI, calls handleOpenAIChat in-process,
// then translates the SSE response back to Gemini format.
func (h *Handler) agiStreamChat(w http.ResponseWriter, r *http.Request, body []byte) {
	// Phase 1 stub: log the request and return a placeholder SSE stream so the
	// IDE doesn't hang. Real translation is implemented in phase 2.
	logger.Infof("[AGI-Inbound] streamGenerateContent (%d bytes) — STUB", len(body))

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	// Emit a single-frame placeholder so the IDE sees a valid response shape.
	placeholder := map[string]interface{}{
		"response": map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"role":  "model",
						"parts": []map[string]interface{}{{"text": "[OmniProxy AGI stub: translation not yet implemented]"}},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]interface{}{
				"promptTokenCount":     0,
				"candidatesTokenCount": 0,
			},
		},
	}
	data, _ := json.Marshal(placeholder)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

