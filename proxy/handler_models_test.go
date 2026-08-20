package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"testing"
)

// modelIDsFromList extracts the "id" field of each entry in a /v1/models
// response payload.
func modelIDsFromList(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /v1/models response: %v", err)
	}
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestHandleModelsExposesOnlyCanonicalClaude5Models(t *testing.T) {
	mustInitConfig(t)
	if err := config.SetExtraModels([]string{"claude-sonnet-5", "claude-opus-4.8", "claude-fable-5"}); err != nil {
		t.Fatalf("SetExtraModels: %v", err)
	}
	defer config.SetExtraModels(nil)

	h := &Handler{}
	// Public discovery must not depend on the upstream cache or ExtraModels.
	h.modelsCacheMu.Lock()
	h.cachedModels = []ModelInfo{{ModelId: "gpt-5.6-luna"}, {ModelId: "claude-opus-4.8-thinking-thinking"}}
	h.modelsCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	ids := modelIDsFromList(t, rec.Body.Bytes())
	want := []string{"claude-sonnet-5", "claude-opus-5"}
	if len(ids) != len(want) {
		t.Fatalf("expected exactly %d public models, got %v", len(want), ids)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("model %d = %q, want %q; full list=%v", i, ids[i], id, ids)
		}
	}
}

func TestHandleModelsWithoutExtraModelsOmitsThem(t *testing.T) {
	mustInitConfig(t)
	if err := config.SetExtraModels(nil); err != nil {
		t.Fatalf("SetExtraModels: %v", err)
	}

	h := &Handler{}
	h.modelsCacheMu.Lock()
	h.cachedModels = nil
	h.modelsCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)

	ids := modelIDsFromList(t, rec.Body.Bytes())
	for _, id := range ids {
		if id == "claude-opus-4.8" || id == "claude-opus-4.8-thinking" || id == "claude-fable-5" || id == "claude-fable-5-thinking" {
			t.Fatalf("unexpected extra model %q when ExtraModels is empty: %v", id, ids)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected canonical Claude catalog, got %v", ids)
	}
}

func TestHandleModelsPreservesKiroProviderMetadata(t *testing.T) {
	mustInitConfig(t)

	h := &Handler{}
	h.modelsCacheMu.Lock()
	h.cachedModels = []ModelInfo{{
		ModelId:    "claude-sonnet-5",
		Provider:   "kiro-proxy",
		InputTypes: []string{"text"},
	}}
	h.modelsCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	h.handleModels(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode /v1/models response: %v", err)
	}

	ownedBy := make(map[string]string, len(response.Data))
	for _, model := range response.Data {
		ownedBy[model.ID] = model.OwnedBy
	}
	for _, id := range []string{"claude-sonnet-5", "claude-opus-5"} {
		if got := ownedBy[id]; got != "anthropic" {
			t.Fatalf("model %q owned_by = %q, want %q", id, got, "anthropic")
		}
	}
}

func TestClaudeModelDiscoveryOmitsHaiku(t *testing.T) {
	mustInitConfig(t)
	if err := config.SetExtraModels([]string{"claude-haiku-5"}); err != nil {
		t.Fatalf("SetExtraModels: %v", err)
	}
	defer config.SetExtraModels(nil)

	h := &Handler{}
	h.modelsCacheMu.Lock()
	h.cachedModels = nil
	h.modelsCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d: %s", rec.Code, rec.Body.String())
	}

	var list struct {
		Data []struct {
			ID          string `json:"id"`
			TokenLimits struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			} `json:"token_limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode /v1/models response: %v", err)
	}

	for _, model := range list.Data {
		if model.ID == "claude-haiku-5" {
			t.Fatalf("Haiku leaked into /v1/models: %#v", model)
		}
	}

	byIDReq := httptest.NewRequest(http.MethodGet, "/v1/models/claude-haiku-5", nil)
	byIDRec := httptest.NewRecorder()
	h.handleModelByID(byIDRec, byIDReq, "claude-haiku-5")
	if byIDRec.Code != http.StatusNotFound {
		t.Fatalf("/v1/models/claude-haiku-5 status = %d, want 404: %s", byIDRec.Code, byIDRec.Body.String())
	}
}

func TestClaudeModelDiscoveryIgnoresStaleHaikuMetadata(t *testing.T) {
	mustInitConfig(t)

	staleLimits := &struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	}{MaxInputTokens: 200_000, MaxOutputTokens: 100}
	h := &Handler{cachedModels: []ModelInfo{{
		ModelId:     "claude-haiku-5",
		Provider:    "anthropic",
		TokenLimits: staleLimits,
	}}}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d: %s", rec.Code, rec.Body.String())
	}

	var list struct {
		Data []struct {
			ID          string `json:"id"`
			TokenLimits struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			} `json:"token_limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode /v1/models response: %v", err)
	}

	for _, model := range list.Data {
		if model.ID == "claude-haiku-5" {
			t.Fatalf("stale Haiku metadata leaked into public catalog: %#v", model)
		}
	}
}

func TestPublicClaudeCatalogOmitsCodexModels(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{cachedModels: []ModelInfo{{
		ModelId:    "gpt-5.6-luna",
		Provider:   "external",
		InputTypes: []string{"text"},
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 200, MaxOutputTokens: 100},
	}}}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d: %s", rec.Code, rec.Body.String())
	}

	var list struct {
		Data []struct {
			ID          string `json:"id"`
			OwnedBy     string `json:"owned_by"`
			TokenLimits struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			} `json:"token_limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode /v1/models response: %v", err)
	}

	for _, model := range list.Data {
		if model.ID != "gpt-5.6-luna" {
			continue
		}
		t.Fatalf("Codex model leaked into public Claude catalog: %#v", model)
	}

	byIDReq := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5.6-luna", nil)
	byIDRec := httptest.NewRecorder()
	h.handleModelByID(byIDRec, byIDReq, "gpt-5.6-luna")
	if byIDRec.Code != http.StatusNotFound {
		t.Fatalf("/v1/models/gpt-5.6-luna status = %d, want 404: %s", byIDRec.Code, byIDRec.Body.String())
	}
}

// TestHandleModelsCatalogAllExposesDiscoveredFamilies pins the opt-in contract:
// the default response stays the conservative canonical catalog, while
// ?catalog=all publishes account-discovered model IDs verbatim (no renaming,
// no alias invention) so pickers can group them by family.
func TestHandleModelsCatalogAllExposesDiscoveredFamilies(t *testing.T) {
	mustInitConfig(t)

	h := &Handler{cachedModels: []ModelInfo{
		{ModelId: "qwen3.8-max", Provider: "qwen", InputTypes: []string{"text"}},
		{ModelId: "glm-5.3", Provider: "zhipu", InputTypes: []string{"text"}},
		{ModelId: "deepseek-v4-pro", Provider: "deepseek", InputTypes: []string{"text"}},
		{ModelId: "gemini-3.1-pro-preview", Provider: "google", InputTypes: []string{"text", "image"}},
		// Duplicate of a canonical entry: must not appear twice.
		{ModelId: "claude-sonnet-5", Provider: "kiro-proxy", InputTypes: []string{"text"}},
	}}

	defaultRec := httptest.NewRecorder()
	h.handleModels(defaultRec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if defaultRec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d: %s", defaultRec.Code, defaultRec.Body.String())
	}
	for _, id := range modelIDsFromList(t, defaultRec.Body.Bytes()) {
		if id == "qwen3.8-max" || id == "glm-5.3" || id == "deepseek-v4-pro" || id == "gemini-3.1-pro-preview" {
			t.Fatalf("discovered model %q leaked into the default catalog", id)
		}
	}

	allRec := httptest.NewRecorder()
	h.handleModels(allRec, httptest.NewRequest(http.MethodGet, "/v1/models?catalog=all", nil))
	if allRec.Code != http.StatusOK {
		t.Fatalf("/v1/models?catalog=all status = %d: %s", allRec.Code, allRec.Body.String())
	}

	ids := modelIDsFromList(t, allRec.Body.Bytes())
	counts := make(map[string]int, len(ids))
	for _, id := range ids {
		counts[id]++
	}
	for _, id := range []string{
		"claude-sonnet-5",
		"claude-opus-5",
		"qwen3.8-max",
		"glm-5.3",
		"deepseek-v4-pro",
		"gemini-3.1-pro-preview",
	} {
		if counts[id] == 0 {
			t.Fatalf("model %q missing from ?catalog=all response: %v", id, ids)
		}
		if counts[id] > 1 {
			t.Fatalf("model %q duplicated %d times in ?catalog=all response", id, counts[id])
		}
	}
}
