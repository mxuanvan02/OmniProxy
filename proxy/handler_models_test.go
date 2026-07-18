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
	if err := config.SetExtraModels([]string{"claude-sonnet-5", "claude-opus-4.8"}); err != nil {
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
