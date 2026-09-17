package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"strings"
	"time"
)

// probeOnce sends a single probe request for one model and records the outcome.
// model may be empty for moderation, which needs no model ID.
//
// This is the only place a probe touches the network, so it is also the only
// place that has to get the dialect right: posting a chat-completions body to a
// Responses gateway reads as "no chat" on a perfectly healthy account.
func (h *Handler) probeOnce(account *config.Account, capability, model, path, credential string) config.CapabilityProbeResult {
	result := config.CapabilityProbeResult{CheckedAt: time.Now().Unix(), Model: model}

	body, ok := probeRequestBody(capability, model)
	if !ok {
		result.Skipped = true
		result.SkippedReason = fmt.Sprintf("%s cannot be probed with a synthetic request", capability)
		result.Detail = result.SkippedReason
		return result
	}

	endpoint := openAICompatibleEndpoint(account.BaseURL, path)
	dialect := externalAPIDialect(account)
	if capability == capabilityChat || capability == capabilityVision {
		// A Responses or Messages gateway must be probed in its own dialect:
		// posting a chat-completions body to one reads as "no chat" on a
		// healthy account, which the matrix reports as missing vision too.
		// Vision rides the same wire, so it shares the endpoint and auth; only
		// the body differs (it carries the image).
		endpoint = probeChatEndpoint(account, dialect)
		var dialectBody []byte
		if capability == capabilityVision {
			dialectBody = probeVisionRequestBody(dialect, model)
		} else {
			dialectBody = probeChatRequestBody(dialect, model)
		}
		if dialectBody != nil {
			body = dialectBody
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	if capability == capabilityChat || capability == capabilityVision {
		applyChatProbeAuth(req, account, credential, dialect)
	} else {
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
	}

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
