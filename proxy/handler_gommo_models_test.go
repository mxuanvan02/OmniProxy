package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"testing"
)

// gommoAdminAccount registers a Gommo media account in config and returns the
// reloaded pool, which is what both admin endpoints resolve IDs through.
func gommoAdminAccount(t *testing.T, id, baseURL string) *accountpool.AccountPool {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           id,
		Email:        "media@example.test",
		AuthMethod:   gommoAuthMethod,
		Provider:     gommoProviderLabel,
		AccessToken:  "tok-abc",
		GommoDomain:  "example.test",
		BaseURL:      baseURL,
		Capabilities: []string{capabilityImage, capabilityVideo, capabilityAudioTTS},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return p
}

// Gommo has no chat capability, but its media catalog lands in the routing
// cache, so the admin test modal used to offer grok_video_heavy and hailuo_2_3
// as chat models.
func TestAccountModelsCachedReportsNoChatModelsForGommo(t *testing.T) {
	const accountID = "gommo-cached"
	p := gommoAdminAccount(t, accountID, "")
	p.SetModelList(accountID, []string{"grok_video_heavy", "hailuo_2_3", "imagegen_2_0"})

	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiGetAccountModelsCached(rec, httptest.NewRequest(http.MethodGet, "/accounts/"+accountID+"/models/cached", nil), accountID)

	var resp struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("models = %v, want none: Gommo exposes no chat models", resp.Models)
	}
}

// /image-models must read the image partition of the Gommo catalog. Without
// that it fell through to the generic image-capability branch, which offered
// the chat account's gpt-5.6-luna and a hardcoded openai/gpt-5-image.
func TestAccountImageModelsReadsOnlyGommoImagePartition(t *testing.T) {
	var types []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		types = append(types, form.Get("type"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"model":"imagegen_2_0","title":"ImageGen 2.0"}]}`))
	}))
	defer server.Close()

	const accountID = "gommo-image-models"
	p := gommoAdminAccount(t, accountID, server.URL)

	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiGetAccountImageModels(rec, httptest.NewRequest(http.MethodGet, "/accounts/"+accountID+"/image-models", nil), accountID)

	if len(types) != 1 || types[0] != "image" {
		t.Fatalf("queried catalog types = %v, want exactly [image]", types)
	}

	var resp struct {
		Models []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"models"`
		Source    string `json:"source"`
		Supported bool   `json:"supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Source != "gommo:image" || !resp.Supported {
		t.Errorf("source = %q supported = %v, want gommo:image/true", resp.Source, resp.Supported)
	}
	if len(resp.Models) != 1 || resp.Models[0].ID != "imagegen_2_0" {
		t.Fatalf("models = %+v, want only the image partition entry", resp.Models)
	}
	if resp.Models[0].Name != "ImageGen 2.0" {
		t.Errorf("name = %q, want the catalog title", resp.Models[0].Name)
	}
}

// A catalog outage must keep the modal usable: no models, but a custom model is
// still accepted rather than the endpoint erroring out.
func TestAccountImageModelsKeepsCustomModelWhenGommoCatalogFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer server.Close()

	const accountID = "gommo-image-fail"
	p := gommoAdminAccount(t, accountID, server.URL)

	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiGetAccountImageModels(rec, httptest.NewRequest(http.MethodGet, "/accounts/"+accountID+"/image-models", nil), accountID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so the modal can still submit a custom model", rec.Code)
	}
	var resp struct {
		Models        []map[string]interface{} `json:"models"`
		Supported     bool                     `json:"supported"`
		CustomAllowed bool                     `json:"customAllowed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Supported {
		t.Error("supported must be false when the catalog call failed")
	}
	if !resp.CustomAllowed {
		t.Error("customAllowed must stay true so the operator can type a model")
	}
	if len(resp.Models) != 0 {
		t.Errorf("models = %+v, want none", resp.Models)
	}
}

// An id upstream does not know comes back as an empty envelope. Reporting that
// as success with a blank URL made the playground show a finished render that
// never existed, so it has to be a 404.
func TestWriteGommoJobStatusRejectsEmptyJob(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGommoJobStatus(rec, "music", gommoJob{ID: "999999999"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a job upstream does not know", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "job not found") {
		t.Errorf("body = %s, want a not-found error", body)
	}
}

func TestWriteGommoJobStatusOmitsBlankURLWhilePending(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGommoJobStatus(rec, "video", gommoJob{ID: "vid-1", Status: "PROCESSING"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a job still rendering", rec.Code)
	}
	var resp struct {
		Status string   `json:"status"`
		URLs   []string `json:"urls"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "PROCESSING" || len(resp.URLs) != 0 {
		t.Errorf("resp = %+v, want PROCESSING with no urls", resp)
	}
}

// Capabilities partition the pool, so an account whose upstream gained music
// support answered 503 on /v1/music/generations forever: the import form was the
// only place that ever set them.
func TestUpdateAccountEditsGommoCapabilities(t *testing.T) {
	const accountID = "gommo-caps"
	p := gommoAdminAccount(t, accountID, "")

	h := &Handler{pool: p}
	body := `{"capabilities":["image","video","audio-tts","audio-music"],"gommoVoiceID":" voice-7 "}`
	rec := httptest.NewRecorder()
	h.apiUpdateAccount(rec, httptest.NewRequest(http.MethodPut, "/accounts/"+accountID, strings.NewReader(body)), accountID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	updated := p.GetByID(accountID)
	if updated == nil {
		t.Fatal("account missing from the pool after update")
	}
	if !containsFold(updated.Capabilities, capabilityAudioMusic) {
		t.Errorf("capabilities = %v, want audio-music included", updated.Capabilities)
	}
	if updated.GommoVoiceID != "voice-7" {
		t.Errorf("voice id = %q, want the trimmed value", updated.GommoVoiceID)
	}
}

// An empty list must not erase every capability: an account with none is
// invisible to every pool and would silently stop serving.
func TestUpdateAccountKeepsGommoServingWhenCapabilitiesEmptied(t *testing.T) {
	const accountID = "gommo-caps-empty"
	p := gommoAdminAccount(t, accountID, "")

	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiUpdateAccount(rec, httptest.NewRequest(http.MethodPut, "/accounts/"+accountID, strings.NewReader(`{"capabilities":[]}`)), accountID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if updated := p.GetByID(accountID); updated == nil || len(updated.Capabilities) == 0 {
		t.Fatalf("capabilities = %v, want the full media set rather than none", updated.Capabilities)
	}
}
