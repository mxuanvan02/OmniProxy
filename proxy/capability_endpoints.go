package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"sort"
	"strings"
)

const (
	endpointEmbeddings     = "embeddings"
	endpointAudioSpeech    = "audio-speech"
	endpointAudioTranscribe = "audio-transcriptions"
	endpointImageEdit      = "image-edit"
	endpointImageVariation = "image-variation"
	endpointModerations    = "moderations"

	// maxPassthroughResponseBytes bounds an upstream response we buffer in
	// memory. Audio responses are the large case; 32 MiB covers long TTS output
	// while still refusing a runaway body.
	maxPassthroughResponseBytes = 32 << 20
	// maxPassthroughRequestBytes bounds an inbound multipart upload (audio file
	// for transcription, source image for edits).
	maxPassthroughRequestBytes = 64 << 20
)

// capabilityEndpoint describes one OpenAI-compatible passthrough route.
type capabilityEndpoint struct {
	// capability is checked against the account's configured and discovered
	// capability sets.
	capability string
	// upstreamPath is appended to the account BaseURL.
	upstreamPath string
	// usageEndpoint is the label recorded in usage/error telemetry.
	usageEndpoint string
	// binaryResponse is true when the upstream returns raw bytes (audio) rather
	// than JSON, so the body must be streamed through untouched.
	binaryResponse bool
	// multipartRequest is true when the inbound request is multipart/form-data
	// and must be forwarded verbatim including its boundary.
	multipartRequest bool
}

var capabilityEndpoints = map[string]capabilityEndpoint{
	"/v1/embeddings": {
		capability:    capabilityEmbedding,
		upstreamPath:  "/v1/embeddings",
		usageEndpoint: endpointEmbeddings,
	},
	"/v1/moderations": {
		capability:    capabilityModeration,
		upstreamPath:  "/v1/moderations",
		usageEndpoint: endpointModerations,
	},
	"/v1/audio/speech": {
		capability:     capabilityAudioTTS,
		upstreamPath:   "/v1/audio/speech",
		usageEndpoint:  endpointAudioSpeech,
		binaryResponse: true,
	},
	"/v1/audio/transcriptions": {
		capability:       capabilityAudioSTT,
		upstreamPath:     "/v1/audio/transcriptions",
		usageEndpoint:    endpointAudioTranscribe,
		multipartRequest: true,
	},
	"/v1/audio/translations": {
		capability:       capabilityAudioSTT,
		upstreamPath:     "/v1/audio/translations",
		usageEndpoint:    endpointAudioTranscribe,
		multipartRequest: true,
	},
	"/v1/images/edits": {
		capability:       capabilityImage,
		upstreamPath:     "/v1/images/edits",
		usageEndpoint:    endpointImageEdit,
		multipartRequest: true,
	},
	"/v1/images/variations": {
		capability:       capabilityImage,
		upstreamPath:     "/v1/images/variations",
		usageEndpoint:    endpointImageVariation,
		multipartRequest: true,
	},
}

// lookupCapabilityEndpoint resolves a request path to a passthrough route,
// accepting both the /v1-prefixed and bare forms used elsewhere in the router.
func lookupCapabilityEndpoint(path string) (capabilityEndpoint, bool) {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if route, ok := capabilityEndpoints[path]; ok {
		return route, true
	}
	if !strings.HasPrefix(path, "/v1/") {
		if route, ok := capabilityEndpoints["/v1"+path]; ok {
			return route, true
		}
	}
	return capabilityEndpoint{}, false
}

// capabilityRouteMatches reports whether the path is served by the capability
// passthrough table. Kept separate from lookupCapabilityEndpoint so the router
// switch stays readable.
func capabilityRouteMatches(path string) bool {
	_, ok := lookupCapabilityEndpoint(path)
	return ok
}

// modelFromPassthroughBody extracts the model field without consuming the body
// for the caller. Multipart bodies are not parsed; the model is optional there.
func modelFromPassthroughBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// selectCapabilityAccounts returns the ordered candidate set for a capability
// request. Model-aware routing comes first (an exact catalog hit is the
// strongest signal that the upstream can serve the request); the capability
// scan is the fallback for requests that carry no usable model ID.
func (h *Handler) selectCapabilityAccounts(capability, model string, limit int) []config.Account {
	if limit <= 0 {
		limit = 8
	}
	var out []config.Account
	seen := make(map[string]bool)

	appendAccount := func(account config.Account) {
		if seen[account.ID] || len(out) >= limit {
			return
		}
		// Only OpenAI-compatible external providers expose these endpoints.
		// Kiro/Codex accounts speak proprietary protocols and would return
		// misleading errors if we forwarded an embeddings request to them.
		if !isExternalAccount(&account) || strings.TrimSpace(account.BaseURL) == "" {
			return
		}
		if !accountSupportsEndpointCapability(&account, capability) {
			return
		}
		seen[account.ID] = true
		out = append(out, account)
	}

	if model != "" {
		excluded := make(map[string]bool)
		for i := 0; i < limit; i++ {
			candidate := h.pool.GetNextForModelExcluding(model, excluded)
			if candidate == nil {
				break
			}
			excluded[candidate.ID] = true
			appendAccount(*candidate)
		}
	}

	for _, account := range config.GetAccounts() {
		if !account.Enabled {
			continue
		}
		appendAccount(account)
	}
	return out
}

// handleCapabilityPassthrough forwards a capability request to the first
// account that can serve it, failing over on upstream errors.
func (h *Handler) handleCapabilityPassthrough(w http.ResponseWriter, r *http.Request, route capabilityEndpoint) {
	var body []byte
	var model string

	if route.multipartRequest {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxPassthroughRequestBytes))
		if err != nil {
			h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		body = raw
		// The model arrives as a form field; extracting it would require
		// parsing and re-encoding the multipart payload, which risks corrupting
		// the upload. Capability routing alone is sufficient here.
	} else {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxPassthroughRequestBytes))
		if err != nil {
			h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request body is required")
			return
		}
		body = raw
		model = modelFromPassthroughBody(raw)
	}

	candidates := h.selectCapabilityAccounts(route.capability, model, 8)
	if len(candidates) == 0 {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error",
			fmt.Sprintf("No available accounts with %s capability. Enable an OpenAI-compatible provider whose catalog exposes %s models.", route.capability, route.capability))
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	var lastErr error
	var lastAccountID string

	for i := range candidates {
		account := candidates[i]
		lastAccountID = account.ID

		status, header, payload, err := forwardCapabilityRequest(r, &account, route, body)
		if err != nil {
			lastErr = err
			h.pool.RecordError(account.ID, false, route.capability)
			logger.Infof("[Capability] %s via %s failed: %v", route.usageEndpoint, account.Email, err)
			continue
		}
		if status >= 400 {
			lastErr = &serviceHTTPError{Status: status, Body: string(truncateErrBody(payload))}
			isQuota := status == http.StatusPaymentRequired || status == http.StatusTooManyRequests
			h.pool.RecordError(account.ID, isQuota, route.capability)
			logger.Infof("[Capability] %s via %s returned HTTP %d", route.usageEndpoint, account.Email, status)
			continue
		}

		h.pool.RecordSuccess(account.ID, route.capability)
		inputTokens, outputTokens := passthroughUsage(payload, route.binaryResponse)
		h.recordUsage(apiKeyID, account.ID, model, route.usageEndpoint, inputTokens, outputTokens, 0, 0, 0, 0)

		contentType := header.Get("Content-Type")
		if contentType == "" {
			if route.binaryResponse {
				contentType = "application/octet-stream"
			} else {
				contentType = "application/json; charset=utf-8"
			}
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write(payload)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts")
		return
	}
	h.recordError(apiKeyID, lastAccountID, model, route.usageEndpoint, lastErr.Error())
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

// forwardCapabilityRequest performs a single upstream attempt. It returns the
// upstream status, headers, and body so the caller can decide whether to fail
// over; a non-nil error means the request never produced a response.
func forwardCapabilityRequest(parent *http.Request, account *config.Account, route capabilityEndpoint, body []byte) (int, http.Header, []byte, error) {
	credential := strings.TrimSpace(account.AccessToken)
	if credential == "" {
		return 0, nil, nil, fmt.Errorf("account %s has no credential", account.ID)
	}
	endpoint := openAICompatibleEndpoint(account.BaseURL, route.upstreamPath)

	req, err := http.NewRequestWithContext(parent.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if route.multipartRequest {
		// The multipart boundary lives in the inbound Content-Type; rewriting it
		// would break the upload, so forward it verbatim.
		if inbound := parent.Header.Get("Content-Type"); inbound != "" {
			req.Header.Set("Content-Type", inbound)
		}
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "*/*")

	client := GetRestClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPassthroughResponseBytes))
	if readErr != nil {
		return 0, nil, nil, readErr
	}
	return resp.StatusCode, resp.Header, payload, nil
}

// passthroughUsage extracts token counts when the upstream reports them. Binary
// responses carry no usage object, and a provider that omits usage yields zeros
// rather than a fabricated estimate.
func passthroughUsage(payload []byte, binary bool) (int, int) {
	if binary || len(payload) == 0 {
		return 0, 0
	}
	var probe struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return 0, 0
	}
	input := probe.Usage.PromptTokens
	if input == 0 {
		input = probe.Usage.InputTokens
	}
	output := probe.Usage.CompletionTokens
	if output == 0 {
		output = probe.Usage.OutputTokens
	}
	if input == 0 && output == 0 && probe.Usage.TotalTokens > 0 {
		input = probe.Usage.TotalTokens
	}
	return input, output
}

// apiGetCapabilities reports the capability matrix so the admin UI can render
// badges from live data instead of a hard-coded pair of labels.
func (h *Handler) apiGetCapabilities(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()

	type accountCapabilityView struct {
		ID           string                                  `json:"id"`
		Email        string                                  `json:"email,omitempty"`
		Nickname     string                                  `json:"nickname,omitempty"`
		Provider     string                                  `json:"provider,omitempty"`
		Enabled      bool                                    `json:"enabled"`
		Configured   []string                                `json:"configured"`
		Discovered   []string                                `json:"discovered"`
		Effective    []string                                `json:"effective"`
		DiscoveredAt int64                                   `json:"discoveredAt,omitempty"`
		Probes       map[string]config.CapabilityProbeResult `json:"probes,omitempty"`
		Verified     []string                                `json:"verified,omitempty"`
	}

	views := make([]accountCapabilityView, 0, len(accounts))
	counts := make(map[string]int)
	enabledCounts := make(map[string]int)
	verifiedCounts := make(map[string]int)
	probeFailureCounts := make(map[string]int)

	for i := range accounts {
		account := accounts[i]
		effective := effectiveAccountCapabilities(&account)
		verified := make([]string, 0, len(account.CapabilityProbes))
		for capability, probe := range account.CapabilityProbes {
			if probe.OK {
				verified = append(verified, capability)
				if account.Enabled {
					verifiedCounts[capability]++
				}
			} else if account.Enabled {
				probeFailureCounts[capability]++
			}
		}
		sort.SliceStable(verified, func(a, b int) bool {
			return capabilityRank(verified[a]) < capabilityRank(verified[b])
		})
		views = append(views, accountCapabilityView{
			ID:           account.ID,
			Email:        account.Email,
			Nickname:     account.Nickname,
			Provider:     account.Provider,
			Enabled:      account.Enabled,
			Configured:   account.Capabilities,
			Discovered:   account.DiscoveredCapabilities,
			Effective:    effective,
			DiscoveredAt: account.CapabilitiesDiscoveredAt,
			Probes:       account.CapabilityProbes,
			Verified:     verified,
		})
		for _, capability := range effective {
			counts[capability]++
			if account.Enabled {
				enabledCounts[capability]++
			}
		}
	}

	type capabilitySummary struct {
		Capability     string `json:"capability"`
		Accounts       int    `json:"accounts"`
		EnabledAccount int    `json:"enabledAccounts"`
		Endpoint       string `json:"endpoint,omitempty"`
		// Available means "an enabled account advertises this capability". It
		// is derived from the provider catalog, which is aspirational: a model
		// can be listed with no channel behind it, and the endpoint path may
		// return 404. Do not read it as "callable".
		Available bool `json:"available"`
		// VerifiedAccounts counts enabled accounts where a probe actually got a
		// 2xx from the endpoint. This is the claim that means "callable".
		VerifiedAccounts int `json:"verifiedAccounts"`
		// ProbeFailures counts enabled accounts where a probe ran and failed,
		// so an operator can tell "never probed" from "probed and broken".
		ProbeFailures int `json:"probeFailures"`
		// Verified is true only when at least one enabled account passed a probe.
		Verified bool `json:"verified"`
	}

	endpointFor := make(map[string]string, len(capabilityEndpoints))
	for path, route := range capabilityEndpoints {
		if _, exists := endpointFor[route.capability]; !exists {
			endpointFor[route.capability] = path
		}
	}
	endpointFor[capabilityChat] = "/v1/chat/completions"
	endpointFor[capabilitySearch] = "/v1/search"
	endpointFor[capabilityImage] = "/v1/images/generations"

	summary := make([]capabilitySummary, 0, len(discoverableCapabilities))
	for _, capability := range discoverableCapabilities {
		summary = append(summary, capabilitySummary{
			Capability:       capability,
			Accounts:         counts[capability],
			EnabledAccount:   enabledCounts[capability],
			Endpoint:         endpointFor[capability],
			Available:        enabledCounts[capability] > 0,
			VerifiedAccounts: verifiedCounts[capability],
			ProbeFailures:    probeFailureCounts[capability],
			Verified:         verifiedCounts[capability] > 0,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"capabilities": summary,
		"accounts":     views,
	})
}
