package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"testing"
)

func TestProbeRequestBody(t *testing.T) {
	cases := []struct {
		name       string
		capability string
		model      string
		wantOK     bool
		wantFields []string
	}{
		{
			name:       "embedding needs input and model",
			capability: capabilityEmbedding,
			model:      "text-embedding-3-small",
			wantOK:     true,
			wantFields: []string{"input", "model"},
		},
		{
			name:       "moderation works without a model",
			capability: capabilityModeration,
			model:      "",
			wantOK:     true,
			wantFields: []string{"input"},
		},
		{
			name:       "tts needs voice",
			capability: capabilityAudioTTS,
			model:      "gpt-4o-mini-tts",
			wantOK:     true,
			wantFields: []string{"model", "input", "voice"},
		},
		{
			name:       "stt is multipart so cannot be synthesised",
			capability: capabilityAudioSTT,
			model:      "whisper-1",
			wantOK:     false,
		},
		{
			name:       "video has no standard endpoint",
			capability: capabilityVideo,
			model:      "veo-3.1",
			wantOK:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, ok := probeRequestBody(tc.capability, tc.model)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			var decoded map[string]interface{}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("probe body is not valid JSON: %v", err)
			}
			for _, field := range tc.wantFields {
				if _, exists := decoded[field]; !exists {
					t.Errorf("probe body missing field %q: %s", field, string(body))
				}
			}
			if tc.model == "" {
				if _, exists := decoded["model"]; exists {
					t.Errorf("expected no model field when model is empty: %s", string(body))
				}
			}
		})
	}
}

func TestProbeCapabilityIsCheap(t *testing.T) {
	cheap := []string{capabilityEmbedding, capabilityModeration}
	costly := []string{capabilityAudioTTS, capabilityAudioSTT, capabilityImage, capabilityVideo}

	for _, capability := range cheap {
		if !probeCapabilityIsCheap(capability) {
			t.Errorf("%s should be cheap to probe", capability)
		}
	}
	for _, capability := range costly {
		if probeCapabilityIsCheap(capability) {
			t.Errorf("%s bills real usage and must not be probed automatically", capability)
		}
	}
}

func TestProbeUpstreamPath(t *testing.T) {
	if path, ok := probeUpstreamPath(capabilityEmbedding); !ok || path != "/v1/embeddings" {
		t.Errorf("embedding path = %q ok=%v", path, ok)
	}
	if path, ok := probeUpstreamPath(capabilityChat); !ok || path != "/v1/chat/completions" {
		t.Errorf("chat path = %q ok=%v", path, ok)
	}
	// Multipart routes must not be selected: a synthetic probe cannot invent a
	// valid file upload, so reporting them as probeable would be misleading.
	if path, ok := probeUpstreamPath(capabilityAudioSTT); ok {
		t.Errorf("audio-stt is multipart and should not be probeable, got %q", path)
	}
	if _, ok := probeUpstreamPath(capabilityVideo); ok {
		t.Error("video has no OpenAI-compatible endpoint and should not be probeable")
	}
}

func TestApplyProbeResultChangeDetection(t *testing.T) {
	account := &config.Account{ID: "acct-1"}

	first := config.CapabilityProbeResult{OK: true, Status: 200, Model: "m1", CheckedAt: 100}
	if !applyProbeResult(account, capabilityEmbedding, first) {
		t.Fatal("first probe result should register as a change")
	}
	// Same outcome, newer timestamp: not a meaningful change, so a config write
	// should be avoidable.
	same := config.CapabilityProbeResult{OK: true, Status: 200, Model: "m1", CheckedAt: 200}
	if applyProbeResult(account, capabilityEmbedding, same) {
		t.Error("identical outcome should not be reported as a change")
	}
	if got := account.CapabilityProbes[capabilityEmbedding].CheckedAt; got != 200 {
		t.Errorf("timestamp should still be refreshed, got %d", got)
	}
	// Status flip is a change.
	broken := config.CapabilityProbeResult{OK: false, Status: 503, Model: "m1", CheckedAt: 300}
	if !applyProbeResult(account, capabilityEmbedding, broken) {
		t.Error("status change should be reported")
	}
}

func TestProbeAccountCapabilityDistinguishesOutcomes(t *testing.T) {
	config.Init("")

	cases := []struct {
		name       string
		status     int
		body       string
		wantOK     bool
		wantStatus int
	}{
		{
			name:       "2xx is verified",
			status:     http.StatusOK,
			body:       `{"data":[{"embedding":[0.1]}]}`,
			wantOK:     true,
			wantStatus: 200,
		},
		{
			name:       "listed model with no channel is not verified",
			status:     http.StatusServiceUnavailable,
			body:       `{"error":{"message":"No available channel for model X under group default"}}`,
			wantOK:     false,
			wantStatus: 503,
		},
		{
			name:       "missing endpoint is not verified",
			status:     http.StatusNotFound,
			body:       `{"detail":"Not Found"}`,
			wantOK:     false,
			wantStatus: 404,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()

			handler := &Handler{pool: accountpool.GetPool()}
			account := &config.Account{
				ID:                     "probe-acct",
				AuthMethod:             "external_openai",
				BaseURL:                server.URL,
				AccessToken:            "test-token",
				Enabled:                true,
				DiscoveredCapabilities: []string{capabilityEmbedding},
			}
			handler.pool.SetModelList(account.ID, []string{"text-embedding-3-small"})

			result := handler.probeAccountCapability(account, capabilityEmbedding)

			if result.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (detail=%q)", result.OK, tc.wantOK, result.Detail)
			}
			if result.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", result.Status, tc.wantStatus)
			}
			if result.Model != "text-embedding-3-small" {
				t.Errorf("Model = %q, want the catalog embedding model", result.Model)
			}
			if gotPath != "/v1/embeddings" {
				t.Errorf("probed path = %q", gotPath)
			}
			if gotAuth != "Bearer test-token" {
				t.Errorf("probe did not forward credential correctly")
			}
			if tc.wantOK && result.Detail != "" {
				t.Errorf("successful probe should not carry an error detail, got %q", result.Detail)
			}
			if !tc.wantOK && result.Detail == "" {
				t.Error("failed probe should record why it failed")
			}
			if result.CheckedAt == 0 {
				t.Error("probe must record a timestamp")
			}
		})
	}
}

func TestProbeAccountCapabilityRejectsNonProbeableAccounts(t *testing.T) {
	config.Init("")
	handler := &Handler{pool: accountpool.GetPool()}

	// Kiro/Codex accounts speak proprietary protocols; probing them would send
	// an OpenAI-shaped body to an endpoint that cannot answer it.
	kiro := &config.Account{ID: "kiro-1", AuthMethod: "social", Enabled: true}
	result := handler.probeAccountCapability(kiro, capabilityEmbedding)
	if result.OK {
		t.Error("non-external account must not be reported as verified")
	}
	if !strings.Contains(result.Detail, "OpenAI-compatible") {
		t.Errorf("expected an explanation about provider kind, got %q", result.Detail)
	}
	if !result.Skipped {
		t.Error("a probe that never left the process must be marked skipped")
	}

	// An unreachable catalog is a skip, not a failure: it carries no evidence
	// about whether the capability endpoint works. Note the probe no longer
	// gives up when the pool cache is empty (that produced a false negative for
	// disabled accounts) — it fetches the provider catalog live, so the reason
	// now describes the fetch, not the cache.
	external := &config.Account{
		ID:                     "ext-no-catalog",
		AuthMethod:             "external_openai",
		BaseURL:                "https://example.invalid",
		AccessToken:            "token",
		Enabled:                true,
		DiscoveredCapabilities: []string{capabilityEmbedding},
	}
	result = handler.probeAccountCapability(external, capabilityEmbedding)
	if result.OK {
		t.Error("account without a reachable catalog must not be verified")
	}
	if !result.Skipped {
		t.Error("an unreachable catalog must be recorded as skipped, not failed")
	}
	if !strings.Contains(result.Detail, "catalog") {
		t.Errorf("expected a catalog-related detail, got %q", result.Detail)
	}
	if result.SkippedReason == "" {
		t.Error("a skipped probe must explain why no request was sent")
	}
	if result.Status != 0 {
		t.Errorf("no request was made so status should be 0, got %d", result.Status)
	}
}

// A disabled account has an empty pool cache, yet its provider still lists
// models. The original implementation reported "no model in cached catalog",
// which was indistinguishable from a real endpoint failure. Verify the live
// fetch closes that gap.
func TestProbeAccountCapabilityFallsBackToLiveCatalog(t *testing.T) {
	config.Init("")

	var sawModelsFetch bool
	var probedModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			sawModelsFetch = true
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[{"id":"text-embedding-3-small"}]}`))
		case "/v1/embeddings":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if m, ok := body["model"].(string); ok {
				probedModel = m
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	// Deliberately disabled: this is the case the pool cache cannot serve.
	account := &config.Account{
		ID:                     "ext-disabled",
		AuthMethod:             "external_openai",
		BaseURL:                server.URL,
		AccessToken:            "token",
		Enabled:                false,
		DiscoveredCapabilities: []string{capabilityEmbedding},
	}

	result := handler.probeAccountCapability(account, capabilityEmbedding)
	if !sawModelsFetch {
		t.Error("expected a live catalog fetch when the pool cache is empty")
	}
	if !result.OK {
		t.Errorf("probe should succeed against a working endpoint, got detail %q", result.Detail)
	}
	if result.Skipped {
		t.Error("a probe that reached upstream must not be marked skipped")
	}
	if probedModel != "text-embedding-3-small" {
		t.Errorf("probe should use the model found in the live catalog, got %q", probedModel)
	}
}

func TestProbeAccountCapabilitiesSkipsCostlyByDefault(t *testing.T) {
	config.Init("")

	var probedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probedPaths = append(probedPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{
		ID:          "multi-cap",
		AuthMethod:  "external_openai",
		BaseURL:     server.URL,
		AccessToken: "token",
		Enabled:     true,
		DiscoveredCapabilities: []string{
			capabilityEmbedding,
			capabilityModeration,
			capabilityAudioTTS,
		},
	}
	handler.pool.SetModelList(account.ID, []string{
		"text-embedding-3-small",
		"omni-moderation-latest",
		"gpt-4o-mini-tts",
	})

	results := handler.probeAccountCapabilities(account, false)
	if _, probed := results[capabilityAudioTTS]; probed {
		t.Error("audio-tts bills real usage and must be skipped unless includeCostly is set")
	}
	if _, probed := results[capabilityEmbedding]; !probed {
		t.Error("embedding should be probed by default")
	}
	if _, probed := results[capabilityModeration]; !probed {
		t.Error("moderation should be probed by default")
	}
	for _, path := range probedPaths {
		if path == "/v1/audio/speech" {
			t.Error("costly endpoint was called without opt-in")
		}
	}

	probedPaths = nil
	results = handler.probeAccountCapabilities(account, true)
	if _, probed := results[capabilityAudioTTS]; !probed {
		t.Error("includeCostly=true should probe audio-tts")
	}
}
