package proxy

import (
	"omniproxy/config"
	"sort"
	"strings"
	"time"
)

// Capability identifiers used by endpoint routing. These are intentionally
// distinct from the pool-partitioning capabilities ("chat", "search", "image")
// so that discovery can describe an account without moving it between pools.
const (
	capabilityChat      = "chat"
	capabilitySearch    = "search"
	capabilityImage     = "image"
	capabilityEmbedding = "embedding"
	capabilityAudioSTT  = "audio-stt"
	capabilityAudioTTS  = "audio-tts"
	capabilityModeration = "moderation"
	capabilityVideo      = "video"
	capabilityAudioMusic = "audio-music"
)
// discoverableCapabilities is the ordered list reported by the capabilities
// endpoint. Order is stable so the admin UI renders badges deterministically.
var discoverableCapabilities = []string{
	capabilityChat,
	capabilityEmbedding,
	capabilityImage,
	capabilityAudioSTT,
	capabilityAudioTTS,
	capabilityModeration,
	capabilityVideo,
	capabilityAudioMusic,
	capabilitySearch,
}

// modelCapabilityRule classifies a model ID by substring match. Substring
// matching is deliberate: provider catalogs are free-form strings and resellers
// rename models constantly, so a fixed allow-list of exact IDs goes stale the
// moment an upstream ships a new version.
type modelCapabilityRule struct {
	capability string
	// needles match anywhere in the lowercased model ID.
	needles []string
	// prefixes match only at the start of the lowercased model ID. Used where a
	// bare substring would produce false positives.
	prefixes []string
}

// modelCapabilityRules is evaluated in order; a model may contribute more than
// one capability (for example gpt-4o-transcribe is audio-stt only, while a
// multimodal chat model contributes chat).
//
// Ordering matters for the negative checks in classifyModelCapabilities: audio
// and image rules run before the chat fallback so a TTS model is not also
// reported as a chat model.
var modelCapabilityRules = []modelCapabilityRule{
	{
		capability: capabilityEmbedding,
		needles: []string{
			"embedding", "embed-", "-embed", "text-embedding",
			"bge-", "gte-", "e5-", "nomic-embed", "voyage-",
		},
		prefixes: []string{"bge", "gte", "embed"},
	},
	{
		capability: capabilityAudioSTT,
		needles: []string{
			"whisper", "transcribe", "transcription", "-asr", "asr-",
			"speech-to-text", "stt",
		},
	},
	{
		capability: capabilityAudioTTS,
		needles: []string{
			"-tts", "tts-", "text-to-speech", "speech-synthesis",
		},
		prefixes: []string{"tts"},
	},
	{
		capability: capabilityModeration,
		needles:    []string{"moderation", "-guard", "guard-", "safety-checker"},
	},
	{
		capability: capabilityVideo,
		needles: []string{
			"veo-", "sora", "seedance", "kling", "runway", "wan-video",
			"-video", "video-gen", "hailuo", "luma-",
		},
	},
	{
		capability: capabilityImage,
		needles: []string{
			"gpt-image", "dall-e", "dalle", "-image", "image-gen",
			"flux", "stable-diffusion", "sdxl", "midjourney",
			"imagen", "seedream", "recraft", "ideogram", "qwen-image",
		},
	},
}

// classifyModelCapabilities maps a single model ID to the capabilities it
// implies. An empty result means the model contributed no signal and the caller
// should treat it as chat.
func classifyModelCapabilities(modelID string) []string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	add := func(capability string) {
		if !seen[capability] {
			seen[capability] = true
			out = append(out, capability)
		}
	}
	for _, rule := range modelCapabilityRules {
		matched := false
		for _, needle := range rule.needles {
			if strings.Contains(id, needle) {
				matched = true
				break
			}
		}
		if !matched {
			for _, prefix := range rule.prefixes {
				if strings.HasPrefix(id, prefix) {
					matched = true
					break
				}
			}
		}
		if matched {
			add(rule.capability)
		}
	}
	// A model that matched a non-chat rule is not treated as a chat model.
	// Video/image/audio/embedding endpoints have their own request shapes and
	// routing them through chat completions produces upstream 400s.
	if len(out) == 0 {
		add(capabilityChat)
	}
	return out
}

// discoverCapabilitiesFromModels derives the capability set for an account from
// its own catalog. This replaces the provider-name switch in
// auth/ninerouter_import.go as the source of truth for endpoint routing.
func discoverCapabilitiesFromModels(models []ModelInfo) []string {
	seen := make(map[string]bool)
	for _, model := range models {
		for _, capability := range classifyModelCapabilities(model.ModelId) {
			seen[capability] = true
		}
		// Catalog metadata is a second, independent signal. A provider that
		// reports an image output modality is treated as image-capable even
		// when its model ID carries no recognisable token.
		for _, value := range append(append([]string{}, model.OutputTypes...), model.Modalities...) {
			normalized := strings.ToLower(strings.TrimSpace(value))
			switch {
			case strings.Contains(normalized, "image"):
				seen[capabilityImage] = true
			case strings.Contains(normalized, "audio"), strings.Contains(normalized, "speech"):
				seen[capabilityAudioTTS] = true
			case strings.Contains(normalized, "video"):
				seen[capabilityVideo] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, capability := range discoverableCapabilities {
		if seen[capability] {
			out = append(out, capability)
		}
	}
	// Preserve any capability that is not part of the ordered list so future
	// rules do not silently vanish from the response.
	for capability := range seen {
		if !containsFold(out, capability) {
			out = append(out, capability)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return capabilityRank(out[i]) < capabilityRank(out[j])
	})
	return out
}

func capabilityRank(capability string) int {
	for i, value := range discoverableCapabilities {
		if strings.EqualFold(value, capability) {
			return i
		}
	}
	return len(discoverableCapabilities)
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// accountSupportsEndpointCapability answers whether an account can serve a
// capability-specific endpoint. It consults the explicitly configured
// capabilities first (operator intent wins) and falls back to what discovery
// observed in the provider catalog.
func accountSupportsEndpointCapability(account *config.Account, capability string) bool {
	if account == nil || strings.TrimSpace(capability) == "" {
		return false
	}
	if accountHasCapability(account, capability) {
		return true
	}
	return containsFold(account.DiscoveredCapabilities, capability)
}

// effectiveAccountCapabilities merges configured and discovered capabilities
// for reporting. Configured values come first so the admin UI shows operator
// intent before inference.
func effectiveAccountCapabilities(account *config.Account) []string {
	if account == nil {
		return nil
	}
	out := make([]string, 0, len(account.Capabilities)+len(account.DiscoveredCapabilities)+1)
	if kind := strings.TrimSpace(account.ProviderKind); kind != "" && !strings.EqualFold(kind, "unsupported") {
		out = append(out, strings.ToLower(kind))
	}
	for _, value := range account.Capabilities {
		if normalized := strings.ToLower(strings.TrimSpace(value)); normalized != "" && !containsFold(out, normalized) {
			out = append(out, normalized)
		}
	}
	for _, value := range account.DiscoveredCapabilities {
		if normalized := strings.ToLower(strings.TrimSpace(value)); normalized != "" && !containsFold(out, normalized) {
			out = append(out, normalized)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return capabilityRank(out[i]) < capabilityRank(out[j])
	})
	return out
}

// applyDiscoveredCapabilities records the classification on the account and
// persists it. It returns true when the stored set changed, so callers can skip
// a config write on every refresh cycle.
func applyDiscoveredCapabilities(account *config.Account, models []ModelInfo) bool {
	if account == nil || len(models) == 0 {
		return false
	}
	discovered := discoverCapabilitiesFromModels(models)
	if len(discovered) == 0 {
		return false
	}
	if equalStringSets(account.DiscoveredCapabilities, discovered) {
		return false
	}
	account.DiscoveredCapabilities = discovered
	account.CapabilitiesDiscoveredAt = time.Now().Unix()
	return true
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !containsFold(b, value) {
			return false
		}
	}
	return true
}
