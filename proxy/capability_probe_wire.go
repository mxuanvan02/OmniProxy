package proxy

import (
	"encoding/json"
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
// a few tokens at most, so the cost is effectively zero. Vision belongs here
// despite carrying an image — the probe image is a 16x16 PNG of 79 bytes, and
// the request asks for a single output token, so it bills less than the chat
// probe it shadows.
var cheapProbeCapabilities = []string{
	capabilityEmbedding,
	capabilityModeration,
	capabilityVision,
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
	case capabilityVision:
		// Default (chat-completions) shape; probeOnce rebuilds this for the
		// account's actual dialect. Returning ok=true here is what makes vision
		// pass the "can this be probed with a synthetic body" gate.
		return probeVisionRequestBody("chat", model), true
	default:
		// audio-stt and image edit/variation are multipart uploads; video has no
		// standard OpenAI-compatible endpoint. Probing these would require
		// fabricating binary payloads, so they stay unverified by design.
		return nil, false
	}
}

// probeUpstreamPath maps a capability to the path to probe. Chat is included so
// the matrix can distinguish a reachable chat provider from a dead one. Vision
// rides the chat wire too — it is the chat endpoint with an image in the body —
// so it reports the chat path; the dialect-specific endpoint is resolved later
// in probeOnce.
func probeUpstreamPath(capability string) (string, bool) {
	if capability == capabilityChat || capability == capabilityVision {
		return "/v1/chat/completions", true
	}
	for path, route := range capabilityEndpoints {
		if route.capability == capability && !route.multipartRequest {
			return path, true
		}
	}
	return "", false
}
