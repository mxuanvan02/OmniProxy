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

func TestHandleModelsAdvertisesExtraModels(t *testing.T) {
	mustInitConfig(t)
	if err := config.SetExtraModels([]string{"claude-sonnet-5", "claude-opus-4.8", "claude-fable-5"}); err != nil {
		t.Fatalf("SetExtraModels: %v", err)
	}
	defer config.SetExtraModels(nil)

	h := &Handler{}
	// Force the cached upstream model list to be empty so the response is
	// deterministic and only contains the fallback anthropic models, the
	// hardcoded aliases, and the extra models we just declared.
	h.modelsCacheMu.Lock()
	h.cachedModels = nil
	h.modelsCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	ids := modelIDsFromList(t, rec.Body.Bytes())
	want := map[string]bool{
		"claude-sonnet-5":          true,
		"claude-opus-4.8":          true,
		"claude-sonnet-5-thinking": true,
		"claude-opus-4.8-thinking": true,
		"claude-fable-5":           true,
		"claude-fable-5-thinking":  true,
	}
	got := make(map[string]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("expected %q in /v1/models, got %v", id, ids)
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
		if id == "claude-sonnet-5" || id == "claude-opus-4.8" {
			t.Fatalf("unexpected extra model %q when ExtraModels is empty: %v", id, ids)
		}
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
	for _, id := range []string{"claude-sonnet-5", "claude-sonnet-5-thinking"} {
		if got := ownedBy[id]; got != "kiro-proxy" {
			t.Fatalf("model %q owned_by = %q, want %q", id, got, "kiro-proxy")
		}
	}
}

func TestClaudeModelDiscoveryExposesPolicyTokenLimits(t *testing.T) {
	mustInitConfig(t)
	if err := config.SetExtraModels([]string{"claude-fable-5"}); err != nil {
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

	for _, wantID := range []string{"claude-fable-5", "claude-fable-5-thinking"} {
		found := false
		for _, model := range list.Data {
			if model.ID != wantID {
				continue
			}
			found = true
			if model.TokenLimits.MaxInputTokens != 1_000_000 || model.TokenLimits.MaxOutputTokens != 128_000 {
				t.Fatalf("model %q token limits = %#v, want 1000000/128000", wantID, model.TokenLimits)
			}
		}
		if !found {
			t.Fatalf("model %q missing from /v1/models", wantID)
		}
	}

	byIDReq := httptest.NewRequest(http.MethodGet, "/v1/models/claude-fable-5", nil)
	byIDRec := httptest.NewRecorder()
	h.handleModelByID(byIDRec, byIDReq, "claude-fable-5")
	if byIDRec.Code != http.StatusOK {
		t.Fatalf("/v1/models/claude-fable-5 status = %d: %s", byIDRec.Code, byIDRec.Body.String())
	}
	var byID struct {
		TokenLimits struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		} `json:"token_limits"`
	}
	if err := json.Unmarshal(byIDRec.Body.Bytes(), &byID); err != nil {
		t.Fatalf("decode /v1/models/claude-fable-5 response: %v", err)
	}
	if byID.TokenLimits.MaxInputTokens != 1_000_000 || byID.TokenLimits.MaxOutputTokens != 128_000 {
		t.Fatalf("/v1/models/claude-fable-5 token limits = %#v, want 1000000/128000", byID.TokenLimits)
	}
}

func TestClaudeModelPolicyOverridesStaleDiscoveryMetadata(t *testing.T) {
	mustInitConfig(t)

	staleLimits := &struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	}{MaxInputTokens: 200_000, MaxOutputTokens: 100}
	h := &Handler{cachedModels: []ModelInfo{{
		ModelId:     "claude-fable-5",
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
		if model.ID != "claude-fable-5" && model.ID != "claude-fable-5-thinking" {
			continue
		}
		if model.TokenLimits.MaxInputTokens != 1_000_000 || model.TokenLimits.MaxOutputTokens != 128_000 {
			t.Fatalf("model %q exposed stale token limits: %#v", model.ID, model.TokenLimits)
		}
	}
}

func TestCodexModelCatalogOverridesStaleDiscoveryMetadata(t *testing.T) {
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

	count := 0
	for _, model := range list.Data {
		if model.ID != "gpt-5.6-luna" {
			continue
		}
		count++
		if model.OwnedBy != "openai-codex" ||
			model.TokenLimits.MaxInputTokens != 272_000 ||
			model.TokenLimits.MaxOutputTokens != 128_000 {
			t.Fatalf("GPT-5.6 Luna metadata = %#v, want canonical Codex 272000/128000", model)
		}
	}
	if count != 1 {
		t.Fatalf("/v1/models exposed %d Luna entries, want one", count)
	}

	byIDReq := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5.6-luna", nil)
	byIDRec := httptest.NewRecorder()
	h.handleModelByID(byIDRec, byIDReq, "gpt-5.6-luna")
	if byIDRec.Code != http.StatusOK {
		t.Fatalf("/v1/models/gpt-5.6-luna status = %d: %s", byIDRec.Code, byIDRec.Body.String())
	}
	var byID struct {
		OwnedBy     string `json:"owned_by"`
		TokenLimits struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		} `json:"token_limits"`
	}
	if err := json.Unmarshal(byIDRec.Body.Bytes(), &byID); err != nil {
		t.Fatalf("decode /v1/models/gpt-5.6-luna response: %v", err)
	}
	if byID.OwnedBy != "openai-codex" ||
		byID.TokenLimits.MaxInputTokens != 272_000 ||
		byID.TokenLimits.MaxOutputTokens != 128_000 {
		t.Fatalf("/v1/models/gpt-5.6-luna metadata = %#v, want canonical Codex 272000/128000", byID)
	}
}
