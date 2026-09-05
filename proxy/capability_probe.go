package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"sort"
	"strings"
	"time"
)

// Capability probing exists because discovery from /v1/models answers the wrong
// question. A catalog entry proves the provider *lists* a model, not that a
// channel is wired behind it, and it says nothing about whether the endpoint
// path is implemented at all. Observed failure modes on real resellers:
//
//	503 "No available channel for model X under group default" — listed, no backing
//	404 {"detail":"Not Found"}                                 — endpoint absent
//	403 from the underlying vendor                             — key lacks the scope
//
// So the capability matrix carries two distinct claims: advertised (from the
// catalog) and verified (an endpoint actually answered).

const (
	// probeTimeout bounds a single probe attempt. Probes are diagnostics, not
	// user traffic; a slow provider should not stall the admin request.
	probeTimeout = 20 * time.Second
	// maxProbeDetailBytes caps the stored upstream error text.
	maxProbeDetailBytes = 300
)

// cheapProbeCapabilities are safe to probe automatically: the request bodies are
// a few tokens at most, so the cost is effectively zero.
var cheapProbeCapabilities = []string{
	capabilityEmbedding,
	capabilityModeration,
}

// probeCapabilityIsCheap reports whether a capability can be probed without
// meaningful cost. Audio and image generation bill per second/per image, so they
// are only probed on explicit request.
func probeCapabilityIsCheap(capability string) bool {
	return containsFold(cheapProbeCapabilities, capability)
}

// probeRequestBody builds the minimal valid request for a capability. It returns
// ok=false for capabilities that cannot be probed with a JSON body (multipart
// endpoints need a real file upload, which a synthetic probe should not invent).
func probeRequestBody(capability, model string) ([]byte, bool) {
	switch capability {
	case capabilityEmbedding:
		payload := map[string]interface{}{"input": "ping", "model": model}
		body, err := json.Marshal(payload)
		return body, err == nil
	case capabilityModeration:
		payload := map[string]interface{}{"input": "ping"}
		if strings.TrimSpace(model) != "" {
			payload["model"] = model
		}
		body, err := json.Marshal(payload)
		return body, err == nil
	case capabilityAudioTTS:
		payload := map[string]interface{}{
			"model": model,
			"input": "ping",
			"voice": "alloy",
		}
		body, err := json.Marshal(payload)
		return body, err == nil
	case capabilityChat:
		payload := map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
		body, err := json.Marshal(payload)
		return body, err == nil
	default:
		// audio-stt and image edit/variation are multipart uploads; video has no
		// standard OpenAI-compatible endpoint. Probing these would require
		// fabricating binary payloads, so they stay unverified by design.
		return nil, false
	}
}

// probeUpstreamPath maps a capability to the path to probe. Chat is included so
// the matrix can distinguish a reachable chat provider from a dead one.
func probeUpstreamPath(capability string) (string, bool) {
	if capability == capabilityChat {
		return "/v1/chat/completions", true
	}
	for path, route := range capabilityEndpoints {
		if route.capability == capability && !route.multipartRequest {
			return path, true
		}
	}
	return "", false
}

// pickProbeModel chooses a catalog model belonging to the capability's family.
//
// The pool cache is consulted first, but it is only populated for accounts that
// are enabled: a disabled account has an empty cached catalog even though its
// provider still lists models. Relying on the cache alone therefore produced a
// false negative ("no model in cached catalog") that looked identical to a real
// failure. When the cache misses, fetch the provider catalog directly so a probe
// reports evidence about the endpoint rather than about pool bookkeeping.
//
// The second return value reports whether a live fetch was attempted, so callers
// can distinguish "provider lists nothing for this capability" from "we could not
// reach the catalog at all".
func (h *Handler) pickProbeModel(account *config.Account, capability string) (string, string) {
	if account == nil {
		return "", "account not found"
	}
	if models := h.pool.GetModelList(account.ID); len(models) > 0 {
		for _, id := range models {
			if containsFold(classifyModelCapabilities(id), capability) {
				return id, ""
			}
		}
		// Cache is populated and holds nothing for this capability: that is a
		// definitive answer, no live fetch needed.
		return "", fmt.Sprintf("provider catalog lists no %s model", capability)
	}

	discovered, err := fetchExternalProviderModels(account)
	if err != nil {
		return "", fmt.Sprintf("could not fetch provider catalog: %v", err)
	}
	if len(discovered) == 0 {
		return "", "provider catalog is empty"
	}
	for _, m := range discovered {
		if containsFold(classifyModelCapabilities(m.ModelId), capability) {
			return m.ModelId, ""
		}
	}
	return "", fmt.Sprintf("provider catalog lists no %s model", capability)
}

// probeAccountCapability performs one probe and returns the recorded result. It
// never mutates the account; the caller decides whether to persist.
func (h *Handler) probeAccountCapability(account *config.Account, capability string) config.CapabilityProbeResult {
	now := time.Now().Unix()
	result := config.CapabilityProbeResult{CheckedAt: now}

	if account == nil {
		result.Skipped = true
		result.SkippedReason = "account not found"
		result.Detail = result.SkippedReason
		return result
	}
	if !isExternalAccount(account) || strings.TrimSpace(account.BaseURL) == "" {
		result.Skipped = true
		result.SkippedReason = "capability probing applies to OpenAI-compatible providers only"
		result.Detail = result.SkippedReason
		return result
	}
	credential := strings.TrimSpace(account.AccessToken)
	if credential == "" {
		result.Skipped = true
		result.SkippedReason = "account has no credential"
		result.Detail = result.SkippedReason
		return result
	}

	path, ok := probeUpstreamPath(capability)
	if !ok {
		result.Skipped = true
		result.SkippedReason = fmt.Sprintf("%s has no probeable JSON endpoint", capability)
		result.Detail = result.SkippedReason
		return result
	}

	model, pickReason := h.pickProbeModel(account, capability)
	if model == "" && capability != capabilityModeration {
		// Moderation accepts a bare input; everything else needs a model ID.
		result.Skipped = true
		result.SkippedReason = pickReason
		result.Detail = pickReason
		return result
	}
	result.Model = model

	body, ok := probeRequestBody(capability, model)
	if !ok {
		result.Skipped = true
		result.SkippedReason = fmt.Sprintf("%s cannot be probed with a synthetic request", capability)
		result.Detail = result.SkippedReason
		return result
	}

	endpoint := openAICompatibleEndpoint(account.BaseURL, path)
	if capability == capabilityChat {
		// Honour the account's chat-path override so a provider whose canonical
		// /v1/chat/completions is unreachable is not reported as a dead chat
		// endpoint while its configured path works.
		endpoint = openAICompatibleEndpoint(account.BaseURL, externalChatPath(account))
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")

	client := GetRestClientForProxy(ResolveAccountProxyURL(account))
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		result.LatencyMs = time.Since(started).Milliseconds()
		result.Detail = err.Error()
		return result
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxPassthroughResponseBytes))
	result.LatencyMs = time.Since(started).Milliseconds()
	result.Status = resp.StatusCode
	result.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !result.OK {
		result.Detail = truncateProbeDetail(payload)
	}
	return result
}

// truncateProbeDetail bounds stored upstream error text. Bodies are provider
// error JSON, never credentials, but they can be long.
func truncateProbeDetail(payload []byte) string {
	detail := strings.TrimSpace(string(payload))
	if detail == "" {
		return ""
	}
	if len(detail) > maxProbeDetailBytes {
		return detail[:maxProbeDetailBytes] + "..."
	}
	return detail
}

// applyProbeResult stores a probe outcome on the account, returning true when
// the stored map changed.
func applyProbeResult(account *config.Account, capability string, result config.CapabilityProbeResult) bool {
	if account == nil || strings.TrimSpace(capability) == "" {
		return false
	}
	if account.CapabilityProbes == nil {
		account.CapabilityProbes = make(map[string]config.CapabilityProbeResult)
	}
	previous, existed := account.CapabilityProbes[capability]
	account.CapabilityProbes[capability] = result
	if !existed {
		return true
	}
	return previous.OK != result.OK || previous.Status != result.Status || previous.Model != result.Model
}

// probeAccountCapabilities probes every advertised capability on the account,
// honouring the cheap/expensive split unless includeCostly is set.
func (h *Handler) probeAccountCapabilities(account *config.Account, includeCostly bool) map[string]config.CapabilityProbeResult {
	out := make(map[string]config.CapabilityProbeResult)
	if account == nil {
		return out
	}
	for _, capability := range effectiveAccountCapabilities(account) {
		if capability == capabilitySearch {
			// Search providers speak bespoke protocols handled by search.go.
			continue
		}
		if !includeCostly && !probeCapabilityIsCheap(capability) {
			continue
		}
		if _, probeable := probeUpstreamPath(capability); !probeable {
			continue
		}
		out[capability] = h.probeAccountCapability(account, capability)
	}
	return out
}

// apiProbeAccountCapabilities exposes probing over the admin API. By default it
// probes only the cheap capabilities; ?includeCostly=true opts into audio/image
// probes that bill real usage.
func (h *Handler) apiProbeAccountCapabilities(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Account not found",
		})
		return
	}

	includeCostly := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("includeCostly")), "true")
	results := h.probeAccountCapabilities(account, includeCostly)

	changed := false
	for capability, result := range results {
		if applyProbeResult(account, capability, result) {
			changed = true
		}
	}
	if changed {
		if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
			logger.Infof("[CapabilityProbe] failed to persist results for %s: %v", account.ID, err)
		}
	}

	// Three outcomes, deliberately kept apart. Folding skipped into failed was
	// the original defect: a probe that never left the process says nothing
	// about the endpoint, yet it rendered identically to a 404 from upstream.
	verified := make([]string, 0, len(results))
	failed := make([]string, 0, len(results))
	skipped := make([]string, 0, len(results))
	for capability, result := range results {
		switch {
		case result.OK:
			verified = append(verified, capability)
		case result.Skipped:
			skipped = append(skipped, capability)
		default:
			failed = append(failed, capability)
		}
	}
	sort.Strings(verified)
	sort.Strings(failed)
	sort.Strings(skipped)

	// An empty result set is not a failure: it means the account advertises no
	// capability that this run was willing to probe. Say so explicitly rather
	// than returning a bare {} the caller has to interpret.
	note := ""
	if len(results) == 0 {
		note = "no advertised capability was eligible for probing"
		if !includeCostly {
			note += " (retry with includeCostly=true to include audio and image)"
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"accountId":     account.ID,
		"includeCostly": includeCostly,
		"probes":        results,
		"verified":      verified,
		"failed":        failed,
		"skipped":       skipped,
		"note":          note,
	})
}
