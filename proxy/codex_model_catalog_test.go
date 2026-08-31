package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSyncCodexModelCatalogAddsProxyModelsAndPreservesExistingEntries(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	seed := `{"models":[{"slug":"auto","base_instructions":"preserve me"},{"slug":"gpt-5.4","base_instructions":"existing"}]}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	models := []map[string]interface{}{{
		"id": "claude-opus-5", "name": "Claude Opus 5", "description": "Reasoning model",
		"input_modalities": []string{"text", "image"},
		"token_limits":     map[string]interface{}{"maxInputTokens": 1_000_000},
	}}
	if err := syncCodexModelCatalog(home, models); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]interface{} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]map[string]interface{}{}
	for _, model := range catalog.Models {
		bySlug[model["slug"].(string)] = model
	}
	if bySlug["gpt-5.4"]["base_instructions"] != "existing" {
		t.Fatal("existing catalog metadata was lost")
	}
	claude := bySlug["claude-opus-5"]
	if claude["display_name"] != "Claude Opus 5" || claude["context_window"] != float64(1_000_000) {
		t.Fatalf("Claude metadata not written: %#v", claude)
	}
	if _, ok := claude["supports_image_detail_original"]; ok {
		t.Fatalf("provider model inherited unverified image capability: %#v", claude)
	}
	// Client-mandatory fields must be present, but only at their conservative
	// floor: an empty level list claims no reasoning support at all.
	levels, ok := claude["supported_reasoning_levels"].([]interface{})
	if !ok || len(levels) != 0 {
		t.Fatalf("provider model reasoning levels = %#v, want empty list", claude["supported_reasoning_levels"])
	}
	if instructions, _ := claude["base_instructions"].(string); instructions != "" {
		t.Fatal("provider model inherited another model's base instructions")
	}
	for key, want := range map[string]interface{}{
		"supports_reasoning_summaries": false,
		"supports_parallel_tool_calls": false,
		"support_verbosity":            false,
	} {
		if got := claude[key]; got != want {
			t.Fatalf("provider model %q = %#v, want %#v", key, got, want)
		}
	}
}

// The Codex client rejects the whole file when a mandatory field is missing, so
// every managed entry must carry the full required set.
func TestSyncCodexModelCatalogWritesClientRequiredFields(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexModelCatalog(home, []map[string]interface{}{
		{"id": "claude-opus-5", "input_modalities": []string{"text", "image"}},
		{"id": "nemotron-ultra-550b", "owned_by": "openai-codex"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]interface{} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("catalog holds %d models, want 2", len(catalog.Models))
	}
	for _, model := range catalog.Models {
		for key := range requiredCodexCatalogFields() {
			if _, ok := model[key]; !ok {
				t.Fatalf("model %v is missing client-required field %q", model["slug"], key)
			}
		}
	}
}

// A genuine value must survive the fill; the floors apply only to gaps.
func TestApplyRequiredCodexCatalogFieldsPreservesRealValues(t *testing.T) {
	entry := map[string]interface{}{
		"supported_reasoning_levels": []interface{}{map[string]interface{}{"effort": "high"}},
		"base_instructions":          "genuine Codex instructions",
		"support_verbosity":          true,
	}
	applyRequiredCodexCatalogFields(entry)
	if levels, _ := entry["supported_reasoning_levels"].([]interface{}); len(levels) != 1 {
		t.Fatalf("real reasoning levels were overwritten: %#v", entry["supported_reasoning_levels"])
	}
	if entry["base_instructions"] != "genuine Codex instructions" {
		t.Fatal("real base instructions were overwritten")
	}
	if entry["support_verbosity"] != true {
		t.Fatal("real capability flag was downgraded")
	}
	if entry["shell_type"] != "shell_command" {
		t.Fatalf("absent field was not filled: %#v", entry["shell_type"])
	}
}

func TestApplyCodexCatalogMetadataClearsUnsupportedTemplateCapabilities(t *testing.T) {
	entry := map[string]interface{}{
		"supports_image_detail_original": true,
		"supports_search_tool":           true,
		"supported_reasoning_levels":     []interface{}{"high"},
	}
	applyCodexCatalogMetadata(entry, "text-only", map[string]interface{}{
		"id":               "text-only",
		"owned_by":         "external",
		"input_modalities": []string{"text"},
		"token_limits":     map[string]interface{}{},
	})
	for _, key := range []string{"supports_image_detail_original", "supports_search_tool", "supported_reasoning_levels", "context_window"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("text-only model retained unverified %q: %#v", key, entry)
		}
	}
	if got := entry["input_modalities"]; !reflect.DeepEqual(got, []interface{}{"text"}) {
		t.Fatalf("input modalities = %#v, want text-only", got)
	}
}

func TestApplyCodexCatalogMetadataDoesNotTrustExternalOwnedBy(t *testing.T) {
	entry := map[string]interface{}{
		"supports_image_detail_original": true,
		"supports_search_tool":           true,
		"supported_reasoning_levels":     []interface{}{"high"},
	}
	applyCodexCatalogMetadata(entry, "nemotron-ultra-550b", map[string]interface{}{
		"id":               "nemotron-ultra-550b",
		"owned_by":         "openai-codex",
		"input_modalities": []string{"text"},
	})
	for _, key := range []string{"supports_image_detail_original", "supports_search_tool", "supported_reasoning_levels"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("external model retained unverified %q: %#v", key, entry)
		}
	}
}

func TestSyncCodexModelCatalogReplacesExistingUnverifiedCapabilities(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	seed := `{"models":[{"slug":"glm-5.3","supports_image_detail_original":true,"supports_search_tool":true,"supported_reasoning_levels":[{"effort":"high"}],"context_window":128000}]}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexModelCatalog(home, []map[string]interface{}{{
		"id": "glm-5.3", "owned_by": "external", "input_modalities": []string{"text"},
	}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]interface{} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	model := catalog.Models[0]
	for _, key := range []string{"supports_image_detail_original", "supports_search_tool", "context_window"} {
		if _, ok := model[key]; ok {
			t.Fatalf("existing model retained unverified %q: %#v", key, model)
		}
	}
	// This field is client-mandatory, so it cannot simply be dropped. The
	// unverified value from the seed must still be discarded: an empty list
	// advertises no reasoning support, which is the honest floor.
	levels, ok := model["supported_reasoning_levels"].([]interface{})
	if !ok || len(levels) != 0 {
		t.Fatalf("unverified reasoning levels survived: %#v", model["supported_reasoning_levels"])
	}
}

func TestSyncCodexModelCatalogRemovesStaleManagedEntries(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	seed := `{"models":[{"slug":"auto"},{"slug":"stale-model","description":"stale-model via OmniProxy."},{"slug":"user-model","description":"Keep this entry"}]}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexModelCatalog(home, []map[string]interface{}{{
		"id": "active-model", "input_modalities": []string{"text"},
	}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]interface{} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]map[string]interface{}, len(catalog.Models))
	for _, model := range catalog.Models {
		bySlug[model["slug"].(string)] = model
	}
	if _, ok := bySlug["stale-model"]; ok {
		t.Fatalf("stale managed model remained in catalog: %#v", bySlug["stale-model"])
	}
	if _, ok := bySlug["user-model"]; !ok {
		t.Fatal("unmanaged catalog entry was removed")
	}
	if managed, _ := bySlug["active-model"]["omniproxy_managed"].(bool); !managed {
		t.Fatal("active model was not marked as OmniProxy managed")
	}
}
