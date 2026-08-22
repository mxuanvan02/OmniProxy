package proxy

import (
	"omniproxy/config"
	"strings"
	"testing"
)

func TestClassifyModelCapabilities(t *testing.T) {
	cases := []struct {
		model string
		want  []string
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
