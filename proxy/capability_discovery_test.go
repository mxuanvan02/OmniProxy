package proxy

import (
	"omniproxy/config"
	"strings"
	"testing"
)

func TestClassifyModelCapabilities(t *testing.T) {
	cases := []struct {
		model   string
		want    []string
		notWant []string
	}{
		{model: "text-embedding-3-large", want: []string{capabilityEmbedding}, notWant: []string{capabilityChat}},
		{model: "qwen3-embedding", want: []string{capabilityEmbedding}, notWant: []string{capabilityChat}},
		{model: "bge-m3", want: []string{capabilityEmbedding}, notWant: []string{capabilityChat}},
		{model: "whisper-1", want: []string{capabilityAudioSTT}, notWant: []string{capabilityChat, capabilityAudioTTS}},
		{model: "gpt-4o-transcribe", want: []string{capabilityAudioSTT}, notWant: []string{capabilityChat}},
		{model: "gpt-4o-mini-tts", want: []string{capabilityAudioTTS}, notWant: []string{capabilityChat, capabilityAudioSTT}},
		{model: "qwen3-tts", want: []string{capabilityAudioTTS}, notWant: []string{capabilityChat}},
		{model: "gpt-image-2", want: []string{capabilityImage}, notWant: []string{capabilityChat}},
		{model: "dall-e-3", want: []string{capabilityImage}, notWant: []string{capabilityChat}},
		{model: "omni-moderation-latest", want: []string{capabilityModeration}, notWant: []string{capabilityChat}},
		{model: "veo-3.1", want: []string{capabilityVideo}, notWant: []string{capabilityChat}},
		{model: "claude-opus-5", want: []string{capabilityChat}, notWant: []string{capabilityEmbedding, capabilityImage}},
		{model: "gpt-5.6-luna", want: []string{capabilityChat}, notWant: []string{capabilityImage}},
		{model: "model-S", want: []string{capabilityChat}, notWant: []string{capabilityEmbedding}},
	}

	for _, tc := range cases {
		got := classifyModelCapabilities(tc.model)
		for _, want := range tc.want {
			if !containsFold(got, want) {
				t.Errorf("model %q: want capability %q, got %v", tc.model, want, got)
			}
		}
		for _, avoid := range tc.notWant {
			if containsFold(got, avoid) {
				t.Errorf("model %q: must not classify as %q, got %v", tc.model, avoid, got)
			}
		}
	}
}

func TestDiscoverCapabilitiesFromModels(t *testing.T) {
	models := []ModelInfo{
		{ModelId: "claude-opus-5"},
		{ModelId: "text-embedding-3-small"},
		{ModelId: "whisper-1"},
		{ModelId: "gpt-4o-mini-tts"},
		{ModelId: "gpt-image-1.5"},
		{ModelId: "omni-moderation"},
	}

	got := discoverCapabilitiesFromModels(models)
	for _, want := range []string{
		capabilityChat, capabilityEmbedding, capabilityAudioSTT,
		capabilityAudioTTS, capabilityImage, capabilityModeration,
	} {
		if !containsFold(got, want) {
			t.Errorf("want %q in discovered set, got %v", want, got)
		}
	}

	// Empty catalog must not invent capabilities.
	if out := discoverCapabilitiesFromModels(nil); len(out) != 0 {
		t.Errorf("empty catalog: want no capabilities, got %v", out)
	}
}

func TestApplyDiscoveredCapabilitiesIdempotent(t *testing.T) {
	account := &config.Account{ID: "acc-1", Email: "a@example.test"}
	models := []ModelInfo{{ModelId: "text-embedding-3-large"}, {ModelId: "claude-opus-5"}}

	if changed := applyDiscoveredCapabilities(account, models); !changed {
		t.Fatal("first classification must report a change")
	}
	if account.CapabilitiesDiscoveredAt == 0 {
		t.Error("discovery timestamp must be set")
	}
	if !containsFold(account.DiscoveredCapabilities, capabilityEmbedding) {
		t.Errorf("want embedding, got %v", account.DiscoveredCapabilities)
	}

	// Re-running with the same catalog must not report a change, otherwise
	// every refresh cycle rewrites config.
	if changed := applyDiscoveredCapabilities(account, models); changed {
		t.Error("identical catalog must not report a change")
	}

	// A catalog that gains a capability must report a change.
	models = append(models, ModelInfo{ModelId: "whisper-1"})
	if changed := applyDiscoveredCapabilities(account, models); !changed {
		t.Error("expanded catalog must report a change")
	}
}

func TestAccountSupportsEndpointCapability(t *testing.T) {
	// Discovered capability alone is enough for endpoint routing.
	discovered := &config.Account{
		ID:                     "acc-discovered",
		AuthMethod:             "external_openai",
		DiscoveredCapabilities: []string{capabilityChat, capabilityEmbedding},
	}
	if !accountSupportsEndpointCapability(discovered, capabilityEmbedding) {
		t.Error("discovered embedding capability must route")
	}
	if accountSupportsEndpointCapability(discovered, capabilityAudioTTS) {
		t.Error("undiscovered capability must not route")
	}

	// Configured capability still wins (service accounts).
	configured := &config.Account{
		ID:           "acc-configured",
		ProviderKind: "search",
		Capabilities: []string{capabilitySearch},
	}
	if !accountSupportsEndpointCapability(configured, capabilitySearch) {
		t.Error("configured search capability must route")
	}

	if accountSupportsEndpointCapability(nil, capabilityChat) {
		t.Error("nil account must not route")
	}
}

func TestLookupCapabilityEndpoint(t *testing.T) {
	cases := []struct {
		path       string
		capability string
		binary     bool
		multipart  bool
	}{
		{path: "/v1/embeddings", capability: capabilityEmbedding},
		{path: "/embeddings", capability: capabilityEmbedding},
		{path: "/v1/moderations", capability: capabilityModeration},
		{path: "/v1/audio/speech", capability: capabilityAudioTTS, binary: true},
		{path: "/v1/audio/transcriptions", capability: capabilityAudioSTT, multipart: true},
		{path: "/v1/audio/translations", capability: capabilityAudioSTT, multipart: true},
		{path: "/v1/images/edits", capability: capabilityImage, multipart: true},
		{path: "/v1/images/variations", capability: capabilityImage, multipart: true},
	}

	for _, tc := range cases {
		route, ok := lookupCapabilityEndpoint(tc.path)
		if !ok {
			t.Errorf("path %q: expected a capability route", tc.path)
			continue
		}
		if route.capability != tc.capability {
			t.Errorf("path %q: want capability %q, got %q", tc.path, tc.capability, route.capability)
		}
		if route.binaryResponse != tc.binary {
			t.Errorf("path %q: want binaryResponse=%v", tc.path, tc.binary)
		}
		if route.multipartRequest != tc.multipart {
			t.Errorf("path %q: want multipartRequest=%v", tc.path, tc.multipart)
		}
	}

	// Existing routes must not be captured by the passthrough table, or the
	// router switch would shadow the native handlers.
	for _, path := range []string{
		"/v1/chat/completions", "/v1/messages", "/v1/responses",
		"/v1/images/generations", "/v1/search", "/v1/models",
	} {
		if _, ok := lookupCapabilityEndpoint(path); ok {
			t.Errorf("path %q must not be handled by capability passthrough", path)
		}
	}
}

func TestModelFromPassthroughBody(t *testing.T) {
	if got := modelFromPassthroughBody([]byte(`{"model":"text-embedding-3-large","input":"hi"}`)); got != "text-embedding-3-large" {
		t.Errorf("want model from JSON body, got %q", got)
	}
	if got := modelFromPassthroughBody([]byte(`not json`)); got != "" {
		t.Errorf("malformed body must yield empty model, got %q", got)
	}
	if got := modelFromPassthroughBody(nil); got != "" {
		t.Errorf("nil body must yield empty model, got %q", got)
	}
}

func TestCapabilityEndpointPathsAreV1Prefixed(t *testing.T) {
	for path := range capabilityEndpoints {
		if !strings.HasPrefix(path, "/v1/") {
			t.Errorf("route key %q must be /v1-prefixed so the bare form resolves", path)
		}
	}
}

// vsllmShapedAccount reproduces the real account the reconciliation was built
// for: a reseller catalog whose model IDs imply embedding and audio-tts, whose
// gateway answers 403 and 404 on those endpoints, and whose chat models accept
// images that no model ID hints at.
func vsllmShapedAccount() *config.Account {
	return &config.Account{
		ID:                     "vsllm",
		ProviderKind:           "external",
		DiscoveredCapabilities: []string{capabilityChat, capabilityEmbedding, capabilityAudioTTS},
		CapabilityProbes: map[string]config.CapabilityProbeResult{
			capabilityVision:    {OK: true, Status: 200, Model: "auto", CheckedAt: 100},
			capabilityEmbedding: {OK: false, Status: 403, Model: "gemini-embedding-001", CheckedAt: 100},
			capabilityAudioTTS:  {OK: false, Status: 404, Model: "mimo-v2.5-tts-voicedesign", CheckedAt: 100},
		},
	}
}

func TestEffectiveAccountCapabilitiesReconcilesProbeVerdicts(t *testing.T) {
	got := effectiveAccountCapabilities(vsllmShapedAccount())

	// Vision was verified live but no model ID hinted at it, so discovery
	// never tagged it; reconciliation must add it back.
	if !containsFold(got, capabilityVision) {
		t.Errorf("verified vision missing from effective set: %v", got)
	}
	// Embedding and audio-tts were inferred from model names but the endpoints
	// answered 403 and 404; a dead verdict must remove the invented tag.
	if containsFold(got, capabilityEmbedding) {
		t.Errorf("embedding survived a 403 verdict: %v", got)
	}
	if containsFold(got, capabilityAudioTTS) {
		t.Errorf("audio-tts survived a 404 verdict: %v", got)
	}
	// Chat was never probed, so the inferred tag stands.
	if !containsFold(got, capabilityChat) {
		t.Errorf("unprobed chat was wrongly removed: %v", got)
	}
}

func TestEffectiveAccountCapabilitiesIgnoresInconclusiveVerdicts(t *testing.T) {
	account := &config.Account{
		ID:                     "slow-gateway",
		DiscoveredCapabilities: []string{capabilityChat, capabilityEmbedding},
		CapabilityProbes: map[string]config.CapabilityProbeResult{
			// Status 0 is a transport failure — no evidence either way.
			capabilityEmbedding: {OK: false, Status: 0, Detail: "context deadline exceeded", CheckedAt: 100},
			// A skipped probe never left the process.
			capabilityVision: {Skipped: true, SkippedReason: "no vision model", CheckedAt: 100},
		},
	}
	got := effectiveAccountCapabilities(account)

	// A timeout must not strip an inferred tag: absence of evidence is not
	// evidence of absence.
	if !containsFold(got, capabilityEmbedding) {
		t.Errorf("inconclusive timeout removed the embedding tag: %v", got)
	}
	// A skipped vision probe adds nothing.
	if containsFold(got, capabilityVision) {
		t.Errorf("skipped vision probe wrongly added a tag: %v", got)
	}
}

func TestEffectiveAccountCapabilitiesRespectsOperatorOverride(t *testing.T) {
	account := vsllmShapedAccount()
	// Operator hand-tags embedding despite the 403: explicit intent wins, and
	// the tag survives so the operator can point at a specific working model.
	account.Capabilities = []string{capabilityEmbedding}
	got := effectiveAccountCapabilities(account)
	if !containsFold(got, capabilityEmbedding) {
		t.Errorf("explicitly configured embedding was stripped by a probe verdict: %v", got)
	}
}

func TestAccountSupportsEndpointCapabilityRefusesDeadEndpoint(t *testing.T) {
	account := vsllmShapedAccount()
	if accountSupportsEndpointCapability(account, capabilityEmbedding) {
		t.Error("routing must skip an account whose embeddings endpoint answered 403")
	}
	if !accountSupportsEndpointCapability(account, capabilityChat) {
		t.Error("chat must still be supported")
	}
	// An explicitly configured capability is operator intent and survives the
	// dead verdict, matching effectiveAccountCapabilities.
	account.Capabilities = []string{capabilityEmbedding}
	if !accountSupportsEndpointCapability(account, capabilityEmbedding) {
		t.Error("explicitly configured embedding must route even after a 403 probe")
	}
}

func TestRawAccountCapabilitiesKeepsDeadTagsProbeable(t *testing.T) {
	// The probe walk reads eligibility from the raw set, so a capability a
	// previous run ruled dead still gets re-probed and can recover once the
	// operator fixes the gateway.
	raw := rawAccountCapabilities(vsllmShapedAccount())
	if !containsFold(raw, capabilityEmbedding) || !containsFold(raw, capabilityAudioTTS) {
		t.Errorf("raw set dropped dead-but-inferred capabilities, freezing their verdict: %v", raw)
	}
}
