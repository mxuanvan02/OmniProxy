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
	capabilityChat       = "chat"
	capabilitySearch     = "search"
	capabilityImage      = "image"
	capabilityEmbedding  = "embedding"
	capabilityAudioSTT   = "audio-stt"
	capabilityAudioTTS   = "audio-tts"
	capabilityModeration = "moderation"
	capabilityVideo      = "video"
	capabilityAudioMusic = "audio-music"
	// capabilityVision is image *input* through the chat endpoint, as distinct
	// from capabilityImage which is image *output* through /v1/images. It rides
	// the chat wire (each dialect carries the image differently) and cannot be
	// inferred from a model ID — qwen3.8-max has no "vision" token yet accepts
	// images — so it is discovered from catalog input-type metadata where the
	// provider publishes any, and otherwise only ever confirmed by a probe.
	capabilityVision = "vision"
)

// discoverableCapabilities is the ordered list reported by the capabilities
// endpoint. Order is stable so the admin UI renders badges deterministically.
var discoverableCapabilities = []string{
	capabilityChat,
	capabilityVision,
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
	// excludeNeedles veto the rule when present. Needed where two capabilities
	// share a word: "speech" appears in both text-to-speech and
	// speech-to-text, so the TTS rule vetoes on the transcription tokens
	// instead of relying on rule ordering (classify evaluates every rule and
	// a model can otherwise collect both capabilities).
	excludeNeedles []string
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
			// Reseller catalogs label TTS with a bare "speech" token
			// (minimax-speech-2-8-hd, autoai-speech-1). The STT vetoes below
			// keep transcription models out.
			"speech", "voicedesign", "voice-design",
		},
		prefixes:       []string{"tts"},
		excludeNeedles: []string{"speech-to-text", "transcribe", "transcription", "whisper", "asr"},
	},
	{
		// Music generation has its own request shape and its own upstream
		// route, so it must not fall through to the chat default.
		capability: capabilityAudioMusic,
		needles: []string{
			"suno", "music", "musicgen", "lyria", "songgen", "-song",
		},
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
			// Bare "veo"/"video" as a leading token, plus upscalers and
			// reseller-specific families that carry no hyphenated marker.
			"video-upscale", "video-heavy", "pixverse", "vidu", "minimax-video",
		},
		prefixes: []string{"veo", "video"},
	},
	{
		capability: capabilityImage,
		needles: []string{
			"gpt-image", "dall-e", "dalle", "-image", "image-gen",
			"flux", "stable-diffusion", "sdxl", "midjourney",
			"imagen", "seedream", "recraft", "ideogram", "qwen-image",
			// Reseller families whose IDs carry no "image" token at all.
			"imagine", "image-upscale", "nano-banana", "gen-image",
		},
		prefixes: []string{"image", "z-image"},
	},
}

// normalizeModelIDForClassification makes provider naming conventions
// comparable. Reseller catalogs are inconsistent about the separator: the same
// family ships as “veo-3-1“ on one gateway and “veo_3_1“ on another, and
// “google_image_gen_banana“ matches no hyphenated needle at all. Collapsing
// underscores, dots and spaces to hyphens lets one rule set cover every
// spelling instead of duplicating needles per separator.
func normalizeModelIDForClassification(modelID string) string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return ""
	}
	replacer := strings.NewReplacer("_", "-", ".", "-", " ", "-", "/", "-")
	id = replacer.Replace(id)
	// Collapse runs of hyphens so "gpt--image" and "gpt-image" behave alike.
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	return strings.Trim(id, "-")
}

// classifyModelCapabilities maps a single model ID to the capabilities it
// implies. An empty result means the model contributed no signal and the caller
// should treat it as chat.
func classifyModelCapabilities(modelID string) []string {
	// Match against the separator-normalized form so underscore-style
	// catalogs (google_image_gen_banana, veo_3_1, minimax_speech_2_8_hd)
	// are classified identically to their hyphenated equivalents. Before
	// this, every underscore-named media model matched no rule and fell
	// through to the chat default.
	id := normalizeModelIDForClassification(modelID)
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
		vetoed := false
		for _, needle := range rule.excludeNeedles {
			if strings.Contains(id, needle) {
				vetoed = true
				break
			}
		}
		if vetoed {
			continue
		}
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
		// when its model ID carries no recognisable token. InputTypes is the
		// only field that distinguishes vision (image input) from image
		// generation (image output), and most resellers do not populate it —
		// so this path rarely fires for External accounts and vision must be
		// confirmed by probe.
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
		for _, value := range model.InputTypes {
			normalized := strings.ToLower(strings.TrimSpace(value))
			if strings.Contains(normalized, "image") || strings.Contains(normalized, "vision") {
				seen[capabilityVision] = true
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
// capabilities first (operator intent wins), then what discovery observed in
// the provider catalog, and finally refuses the request when a probe has
// proven the endpoint dead.
//
// The dead check exists because the first two signals are both guesses about
// the catalog, while a 404 from /v1/audio/speech is a fact about the gateway.
// Routing an embeddings request to an account whose embeddings endpoint
// answers 403 hands the caller an upstream error instead of failing over to
// an account that can actually serve it.
func accountSupportsEndpointCapability(account *config.Account, capability string) bool {
	if account == nil || strings.TrimSpace(capability) == "" {
		return false
	}
	if probeEvidence(account, capability) == probeEvidenceDead && !accountHasCapability(account, capability) {
		return false
	}
	if accountHasCapability(account, capability) {
		return true
	}
	return containsFold(account.DiscoveredCapabilities, capability)
}

// probeEvidenceClass is what a stored probe result proves about a capability.
//
// The three-way split exists because a probe failure is not one thing. A 404 on
// /v1/audio/speech means the gateway has no such endpoint — that is evidence the
// capability is absent, and advertising it sends real requests to a dead path. A
// timeout means the gateway was slow once — that is no evidence at all, and
// treating it as absence removes a working capability from the matrix because of
// one congested minute. Collapsing the two is what made a slow VSLLM probe run
// erase a vision verdict that had answered 200 seconds earlier.
type probeEvidenceClass int

const (
	// probeEvidenceNone means no usable verdict is stored: never probed, or the
	// probe was skipped because no candidate model could be reached.
	probeEvidenceNone probeEvidenceClass = iota
	// probeEvidenceLive means the upstream answered 2xx for this capability.
	probeEvidenceLive
	// probeEvidenceDead means the upstream answered with a client-error status,
	// which the probe walk only records after its own model-level refusals
	// (429, "no access to model", provider model-unavailable) have been
	// filtered out as skipped.
	probeEvidenceDead
)

// probeResultInconclusive reports whether a result carries no evidence about
// the capability: it never produced an HTTP response (Status 0 — timeout, DNS
// failure, connection reset) or never left the process (Skipped). Every real
// status code, including a 5xx, means the gateway answered and is worth
// recording; whether it proves the capability dead is classifyProbeEvidence's
// question, not this one's.
func probeResultInconclusive(result config.CapabilityProbeResult) bool {
	return result.Skipped || result.Status == 0
}

// classifyProbeEvidence buckets a stored probe result. Only a 4xx other than
// 429 is treated as absence: those are the gateway's own answer about the
// endpoint or the capability, after the probe walk has already filtered
// model-level refusals out as skipped. A 429 is quota, a 5xx is the gateway
// being unwell, and Status 0 never reached it — none of them say the
// capability is missing.
func classifyProbeEvidence(result config.CapabilityProbeResult, exists bool) probeEvidenceClass {
	if !exists || probeResultInconclusive(result) {
		return probeEvidenceNone
	}
	if result.OK {
		return probeEvidenceLive
	}
	if result.Status >= 400 && result.Status < 500 && result.Status != 429 {
		return probeEvidenceDead
	}
	return probeEvidenceNone
}

// probeEvidence looks up the stored verdict for one capability on one account.
func probeEvidence(account *config.Account, capability string) probeEvidenceClass {
	if account == nil || len(account.CapabilityProbes) == 0 {
		return probeEvidenceNone
	}
	for name, result := range account.CapabilityProbes {
		if strings.EqualFold(name, capability) {
			return classifyProbeEvidence(result, true)
		}
	}
	return probeEvidenceNone
}

// rawAccountCapabilities is the union of what the operator configured and what
// catalog-name discovery inferred, before any probe verdict is applied.
//
// Kept separate from effectiveAccountCapabilities on purpose: the probe walk
// decides what to re-probe from this set, so a capability a probe has already
// ruled dead still gets probed again and can recover. Reading eligibility from
// the reconciled set instead would freeze the first dead verdict permanently —
// a gateway whose embeddings endpoint was disabled on Monday could never be
// re-tagged after the operator fixed it on Tuesday.
func rawAccountCapabilities(account *config.Account) []string {
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

// effectiveAccountCapabilities is the capability list the admin UI shows and
// endpoint routing consults: the configured and inferred set, corrected by
// every probe verdict that actually proves something.
//
// Name-based discovery is a guess, and on reseller catalogs it guesses wrong in
// both directions. VSLLM lists gemini-embedding-001 and
// mimo-v2.5-tts-voicedesign, so discovery tagged embedding and audio-tts —
// while the gateway answers 403 on /v1/embeddings and 404 on /v1/audio/speech,
// because it has neither endpoint. The same catalog contains no model whose ID
// hints at image input, so vision was missing from the tags even though a probe
// of claude-opus-4-8 with a 16x16 PNG answered 200 and "Red".
//
// A probe verdict therefore adds a capability discovery missed and removes one
// discovery invented, but only when the verdict is evidence: inconclusive
// results (never probed, skipped, timed out) leave the inferred tag alone, since
// absence of evidence is not evidence of absence. Explicitly configured
// capabilities survive a dead verdict for the same reason operator intent wins
// elsewhere in this file — an operator who hand-tagged an account knows
// something the probe cannot.
func effectiveAccountCapabilities(account *config.Account) []string {
	if account == nil {
		return nil
	}
	raw := rawAccountCapabilities(account)
	out := make([]string, 0, len(raw)+1)
	configured := func(capability string) bool {
		return accountHasCapability(account, capability)
	}
	for _, capability := range raw {
		if probeEvidence(account, capability) == probeEvidenceDead && !configured(capability) {
			continue
		}
		out = append(out, capability)
	}
	// A verified capability that no model ID hinted at still belongs on the
	// account: vision is the common case, because it is a property of a chat
	// model rather than a model family of its own.
	for capability, result := range account.CapabilityProbes {
		normalized := strings.ToLower(strings.TrimSpace(capability))
		if normalized == "" || containsFold(out, normalized) {
			continue
		}
		if classifyProbeEvidence(result, true) == probeEvidenceLive {
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
