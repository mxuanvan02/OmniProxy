package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cliTestModel(id string, input, output int) ModelInfo {
	return ModelInfo{
		ModelId: id,
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: input, MaxOutputTokens: output},
	}
}

func TestApplyClaudeCliSettingsMigratesLegacyAuthToken(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatalf("create Claude settings directory: %v", err)
	}
	legacySettings := `{"env":{"ANTHROPIC_AUTH_TOKEN":"legacy-key","ANTHROPIC_BASE_URL":"http://old-proxy"}}`
	if err := os.WriteFile(settingsPath, []byte(legacySettings), 0644); err != nil {
		t.Fatalf("write legacy Claude settings: %v", err)
	}

	body := strings.NewReader(`{"env":{"ANTHROPIC_BASE_URL":"http://new-proxy/v1","ANTHROPIC_API_KEY":"current-key"}}`)
	req := httptest.NewRequest(http.MethodPost, "/cli-tools/claude", body)
	recorder := httptest.NewRecorder()

	(&Handler{}).apiApplyCliToolSettings(recorder, req, "claude")
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply Claude settings status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read migrated Claude settings: %v", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("decode migrated Claude settings: %v", err)
	}
	if got := settings.Env["ANTHROPIC_API_KEY"]; got != "current-key" {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want current request key", got)
	}
	if _, found := settings.Env["ANTHROPIC_AUTH_TOKEN"]; found {
		t.Fatalf("legacy ANTHROPIC_AUTH_TOKEN was persisted: %v", settings.Env)
	}
	if got := settings.Env["ANTHROPIC_BASE_URL"]; got != "http://new-proxy" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want versionless proxy URL", got)
	}
}

func TestApplyClaudeCliSettingsPreservesModelRouting(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatalf("create Claude settings directory: %v", err)
	}
	initial := map[string]interface{}{
		"model":         "opus",
		"fallbackModel": []string{"sonnet"},
		"advisorModel":  "gpt-5.6-sol",
		"fallbackModels": []string{
			"gpt-5.6-sol",
		},
		"availableModels": []string{"opus", "sonnet", "claude-fable-5"},
		"env": map[string]string{
			"ANTHROPIC_DEFAULT_OPUS_MODEL":   "claude-opus-5[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "claude-sonnet-5",
		},
	}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("encode initial Claude settings: %v", err)
	}
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatalf("write initial Claude settings: %v", err)
	}

	// This is the stale payload previously emitted by the OmniProxy UI.
	body := strings.NewReader(`{"env":{"ANTHROPIC_BASE_URL":"http://new-proxy/v1","ANTHROPIC_API_KEY":"current-key","ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-4.8","ANTHROPIC_DEFAULT_SONNET_MODEL":"claude-sonnet-5","ANTHROPIC_DEFAULT_HAIKU_MODEL":"gpt-5.6-sol"}}`)
	req := httptest.NewRequest(http.MethodPost, "/cli-tools/claude", body)
	recorder := httptest.NewRecorder()

	(&Handler{}).apiApplyCliToolSettings(recorder, req, "claude")
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply Claude settings status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	data, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read Claude settings: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode Claude settings: %v", err)
	}
	if got["model"] != "opus" {
		t.Fatalf("primary model = %v, want opus", got["model"])
	}
	fallback, ok := got["fallbackModel"].([]interface{})
	if !ok || len(fallback) != 1 || fallback[0] != "sonnet" {
		t.Fatalf("primary fallback = %#v, want [sonnet]", got["fallbackModel"])
	}
	if got["advisorModel"] != "gpt-5.6-sol" {
		t.Fatalf("advisor model = %v, want gpt-5.6-sol", got["advisorModel"])
	}
	if _, found := got["fallbackModels"]; found {
		t.Fatalf("legacy fallbackModels key remains: %#v", got["fallbackModels"])
	}
	available, ok := got["availableModels"].([]interface{})
	if !ok {
		t.Fatalf("availableModels has unexpected type %T", got["availableModels"])
	}
	wantAvailable := []interface{}{"opus", "sonnet", "claude-fable-5", "claude-sonnet-5", "claude-opus-5", "model-S", "model-T", "model-O", "model-A"}
	if len(available) != len(wantAvailable) {
		t.Fatalf("availableModels = %#v, want exactly %#v", available, wantAvailable)
	}
	for i, want := range wantAvailable {
		if available[i] != want {
			t.Fatalf("availableModels[%d] = %#v, want %#v; full list=%#v", i, available[i], want, available)
		}
	}
	env, ok := got["env"].(map[string]interface{})
	if !ok {
		t.Fatalf("env has unexpected type %T", got["env"])
	}
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "claude-opus-5[1m]" {
		t.Fatalf("opus env model = %v, stale Apply payload overwrote it", env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "claude-sonnet-5" {
		t.Fatalf("haiku tier model = %v, want claude-sonnet-5", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
	}
}

func TestApplyCodexCliSettingsWritesDesktopModelCatalog(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	// The cache mixes what the picker should gain (non-Codex families) with what
	// it should not: a Codex builtin that already has a richer bundled entry, and
	// a per-effort alias of a model that is listed anyway.
	h := &Handler{cachedModels: []ModelInfo{
		cliTestModel("claude-opus-5", 1_000_000, 128_000),
		cliTestModel("claude-opus-5-max", 1_000_000, 128_000),
		cliTestModel("model-A", 200_000, 100_000),
		cliTestModel("gpt-5.6-sol", 272_000, 128_000),
	}}
	req := httptest.NewRequest(http.MethodPost, "/cli-tools/codex", strings.NewReader(`{"baseUrl":"http://proxy","apiKey":"key","model":"claude-opus-5"}`))
	rec := httptest.NewRecorder()
	h.apiApplyCliToolSettings(rec, req, "codex")
	if rec.Code != http.StatusOK {
		t.Fatalf("apply Codex settings status = %d: %s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(filepath.Join(homeDir, ".codex", "model-catalog.json"))
	if err != nil {
		t.Fatalf("read Codex desktop catalog: %v", err)
	}
	var catalog struct {
		Models []map[string]interface{} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("decode Codex desktop catalog: %v", err)
	}
	var claude map[string]interface{}
	for _, model := range catalog.Models {
		if model["slug"] == "claude-opus-5" {
			claude = model
			break
		}
	}
	if claude == nil {
		t.Fatalf("configured model missing from desktop catalog: %#v", catalog.Models)
	}
	if got := claude["context_window"]; got != float64(1_000_000) {
		t.Fatalf("catalog context_window = %#v, want 1000000", got)
	}
	if managed, _ := claude["omniproxy_managed"].(bool); !managed {
		t.Fatalf("catalog entry is not OmniProxy-managed: %#v", claude)
	}
	// The cache exposes claude-opus-5-max, so this family's Effort menu must
	// reach max instead of stopping at the base set.
	assertReasoningLevels(t, claude, "low", "medium", "high", "max")
	published := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		slug, _ := model["slug"].(string)
		published[slug] = true
	}
	// A discovered non-Codex family is worth adding: Codex has no entry for it.
	if !published["model-A"] {
		t.Fatalf("discovered non-Codex family missing from picker: %#v", catalog.Models)
	}
	// Effort is a UI control, not a separate model.
	if published["claude-opus-5-max"] {
		t.Fatal("per-effort alias was published as its own model")
	}
	// Codex ships gpt-* itself; republishing replaces its metadata with ours.
	if published["gpt-5.6-sol"] {
		t.Fatal("Codex builtin model was republished by OmniProxy")
	}
}

func TestApplyOpenClawSettingsMergesExistingAgentsAndProviderModels(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	path := filepath.Join(homeDir, ".openclaw", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create OpenClaw directory: %v", err)
	}
	existing := map[string]interface{}{
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				"other":     map[string]interface{}{"baseUrl": "http://other"},
				"omniproxy": map[string]interface{}{"models": []interface{}{map[string]interface{}{"id": "old-model"}}},
			},
		},
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{"model": map[string]interface{}{"primary": "omniproxy/old-model"}},
			"list":     []interface{}{map[string]interface{}{"id": "worker", "model": "omniproxy/old-model"}},
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write existing OpenClaw config: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/cli-tools/openclaw", strings.NewReader(`{"baseUrl":"http://proxy","apiKey":"key","model":"claude-sonnet-5","agentModels":{"worker":"kiro-proxy/claude-opus-4.8","new-agent":"gpt-5"}}`))
	rec := httptest.NewRecorder()
	(&Handler{}).apiApplyCliToolSettings(rec, req, "openclaw")
	if rec.Code != http.StatusOK {
		t.Fatalf("apply OpenClaw settings status = %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OpenClaw config: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode OpenClaw config: %v", err)
	}
	agents := got["agents"].(map[string]interface{})
	defaults := agents["defaults"].(map[string]interface{})
	primary := defaults["model"].(map[string]interface{})["primary"]
	if primary != "omniproxy/claude-sonnet-5" {
		t.Fatalf("primary model = %v", primary)
	}
	list := agents["list"].([]interface{})
	seen := map[string]string{}
	for _, raw := range list {
		a := raw.(map[string]interface{})
		seen[a["id"].(string)] = a["model"].(string)
	}
	if seen["worker"] != "kiro-proxy/claude-opus-4.8" || seen["new-agent"] != "omniproxy/gpt-5" {
		t.Fatalf("agent models = %#v", seen)
	}
	providers := got["models"].(map[string]interface{})["providers"].(map[string]interface{})
	if _, ok := providers["other"]; !ok {
		t.Fatal("unrelated provider was removed")
	}
	models := providers["omniproxy"].(map[string]interface{})["models"].([]interface{})
	for _, raw := range models {
		entry := raw.(map[string]interface{})
		if _, found := entry["description"]; found {
			t.Fatalf("OpenClaw provider model %q contains unsupported description: %#v", entry["id"], entry)
		}
	}
}

func TestMergeOpenClawProviderCatalogRemovesLegacyDescriptions(t *testing.T) {
	provider := map[string]interface{}{
		"models": []interface{}{
			map[string]interface{}{
				"id":          "gpt-5.6-luna",
				"name":        "old name",
				"description": "legacy metadata",
			},
			map[string]interface{}{
				"id":          "removed-model",
				"name":        "removed model",
				"description": "legacy metadata",
			},
		},
	}
	mergeOpenClawProviderCatalog(provider, codexSubscriptionModels())

	models := provider["models"].([]interface{})
	foundLuna := false
	for _, raw := range models {
		entry := raw.(map[string]interface{})
		if _, found := entry["description"]; found {
			t.Fatalf("legacy description remains in OpenClaw model: %#v", entry)
		}
		if entry["id"] == "gpt-5.6-luna" {
			foundLuna = true
		}
	}
	if !foundLuna {
		t.Fatal("Luna model missing from OpenClaw provider catalog")
	}
}

func TestApplyHermesSettingsUpdatesLimitsForSelectedModel(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	h := &Handler{cachedModels: []ModelInfo{
		cliTestModel("claude-sonnet-5", 200000, 64000),
		cliTestModel("claude-opus-4.8", 1000000, 128000),
	}}

	apply := func(model string) {
		req := httptest.NewRequest(http.MethodPost, "/cli-tools/hermes", strings.NewReader(`{"baseUrl":"http://proxy","apiKey":"key","model":"`+model+`"}`))
		rec := httptest.NewRecorder()
		h.apiApplyCliToolSettings(rec, req, "hermes")
		if rec.Code != http.StatusOK {
			t.Fatalf("apply Hermes settings for %s: status = %d: %s", model, rec.Code, rec.Body.String())
		}
	}

	apply("claude-sonnet-5")
	configPath := filepath.Join(homeDir, ".hermes", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read Hermes config after first apply: %v", err)
	}
	if got := string(data); !strings.Contains(got, "context_length: 1000000") || !strings.Contains(got, "max_tokens: 128000") {
		t.Fatalf("canonical Claude Sonnet 5 limits missing from Hermes config:\n%s", got)
	}

	apply("claude-opus-4.8")
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read Hermes config after second apply: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "context_length: 1000000") || !strings.Contains(got, "max_tokens: 128000") {
		t.Fatalf("second model limits missing from Hermes config:\n%s", got)
	}
	modelSection := got
	if providersIndex := strings.Index(modelSection, "providers:"); providersIndex >= 0 {
		modelSection = modelSection[:providersIndex]
	}
	if strings.Contains(modelSection, "context_length: 200000") || strings.Contains(modelSection, "max_tokens: 64000") {
		t.Fatalf("stale Claude Sonnet 5 limits remain in Hermes config:\n%s", got)
	}
}

func TestApplyOpenClawSettingsUpdatesLimitsForSelectedModel(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	h := &Handler{cachedModels: []ModelInfo{
		cliTestModel("claude-sonnet-5", 200000, 64000),
		cliTestModel("claude-opus-4.8", 1000000, 128000),
	}}
	apply := func(model string) {
		req := httptest.NewRequest(http.MethodPost, "/cli-tools/openclaw", strings.NewReader(`{"baseUrl":"http://proxy","apiKey":"key","model":"`+model+`"}`))
		rec := httptest.NewRecorder()
		h.apiApplyCliToolSettings(rec, req, "openclaw")
		if rec.Code != http.StatusOK {
			t.Fatalf("apply OpenClaw settings for %s: status = %d: %s", model, rec.Code, rec.Body.String())
		}
	}

	apply("claude-sonnet-5")
	apply("claude-opus-4.8")

	data, err := os.ReadFile(filepath.Join(homeDir, ".openclaw", "openclaw.json"))
	if err != nil {
		t.Fatalf("read OpenClaw config: %v", err)
	}
	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode OpenClaw config: %v", err)
	}
	providers := config["models"].(map[string]interface{})["providers"].(map[string]interface{})
	provider := providers["omniproxy"].(map[string]interface{})
	models := provider["models"].([]interface{})
	var selected map[string]interface{}
	for _, raw := range models {
		entry := raw.(map[string]interface{})
		if entry["id"] == "claude-opus-4.8" {
			selected = entry
			break
		}
	}
	if selected == nil {
		t.Fatalf("selected model is missing from OpenClaw provider: %v", models)
	}
	if selected["contextWindow"] != float64(1000000) || selected["maxTokens"] != float64(128000) {
		t.Fatalf("selected model limits = %#v, want contextWindow=1000000 maxTokens=128000", selected)
	}
}

func TestCanonicalCodexLimitsOverrideStaleCache(t *testing.T) {
	h := &Handler{cachedModels: []ModelInfo{cliTestModel("gpt-5.6-luna", 200, 100)}}
	input, output, ok := h.modelTokenLimits("omniproxy/gpt-5.6-luna")
	if !ok || input != 272_000 || output != 128_000 {
		t.Fatalf("Luna limits = (%d, %d, %v), want (272000, 128000, true)", input, output, ok)
	}
	if got := h.contextWindowForModel("gpt-5.6-luna"); got != 272_000 {
		t.Fatalf("Luna context window = %d, want 272000", got)
	}
}

func TestApplyHermesSettingsUsesCanonicalLunaLimits(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	// This reproduces the bad runtime state reported by Hermes: the cache
	// contains an obsolete 200/100 Luna entry, but the generated config must
	// use the canonical Codex catalog values.
	h := &Handler{cachedModels: []ModelInfo{cliTestModel("gpt-5.6-luna", 200, 100)}}
	req := httptest.NewRequest(http.MethodPost, "/cli-tools/hermes", strings.NewReader(`{"baseUrl":"http://proxy","apiKey":"key","model":"gpt-5.6-luna"}`))
	rec := httptest.NewRecorder()
	h.apiApplyCliToolSettings(rec, req, "hermes")
	if rec.Code != http.StatusOK {
		t.Fatalf("apply Hermes Luna settings: status = %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(filepath.Join(homeDir, ".hermes", "config.yaml"))
	if err != nil {
		t.Fatalf("read Hermes Luna config: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "context_length: 272000") || !strings.Contains(got, "max_tokens: 128000") {
		t.Fatalf("canonical Luna limits missing from Hermes config:\n%s", got)
	}
	if strings.Contains(got, "context_length: 200\n") || strings.Contains(got, "max_tokens: 100\n") {
		t.Fatalf("stale Luna limits remain in Hermes config:\n%s", got)
	}
}

func TestModelInfoTokenLimitsFillsOnlyMissingMetadata(t *testing.T) {
	input, output, ok := modelInfoTokenLimits(ModelInfo{
		ModelId: "gpt-5.6-luna",
		TokenLimits: &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: 180_000},
	})
	if !ok || input != 180_000 || output != 128_000 {
		t.Fatalf("partial Luna metadata = (%d, %d, %v), want (180000, 128000, true)", input, output, ok)
	}
}

// claudeSettingsAfterApply runs Apply against a redirected HOME and returns the
// settings file it wrote.
func claudeSettingsAfterApply(t *testing.T, seed string, body string) map[string]interface{} {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if seed != "" {
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
			t.Fatalf("create Claude settings directory: %v", err)
		}
		if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
			t.Fatalf("seed Claude settings: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/cli-tools/claude", strings.NewReader(body))
	rec := httptest.NewRecorder()
	(&Handler{}).apiApplyCliToolSettings(rec, req, "claude")
	if rec.Code != http.StatusOK {
		t.Fatalf("apply Claude settings status = %d: %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read Claude settings: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode Claude settings: %v", err)
	}
	return got
}

func claudePickerRow(t *testing.T, settings map[string]interface{}, model string) map[string]interface{} {
	t.Helper()
	picker, _ := settings["modelPicker"].(map[string]interface{})
	if picker == nil {
		t.Fatalf("modelPicker missing: %#v", settings)
	}
	options, _ := picker["options"].([]interface{})
	for _, raw := range options {
		row, _ := raw.(map[string]interface{})
		if row != nil && row["model"] == model {
			return row
		}
	}
	t.Fatalf("picker row for %q missing: %#v", model, options)
	return nil
}

// A SOTA alias is an ID Claude Code does not know, so it offers no Effort
// control for it. behavesAs on the picker row is the channel that supplies the
// capability profile; without a row the alias routes but cannot be selected.
func TestApplyClaudeCliSettingsPublishesSotaPickerRowsWithBehavesAs(t *testing.T) {
	got := claudeSettingsAfterApply(t, "", `{"env":{"ANTHROPIC_BASE_URL":"http://proxy/v1","ANTHROPIC_API_KEY":"key"}}`)
	for model, want := range map[string]string{
		"model-S": "claude-fable-5",
		"model-T": "claude-sonnet-5",
		"model-O": "claude-opus-5",
		"model-A": "claude-opus-5",
	} {
		row := claudePickerRow(t, got, model)
		if row["behavesAs"] != want {
			t.Errorf("%s behavesAs = %#v, want %q", model, row["behavesAs"], want)
		}
	}
}

func TestApplyClaudeCliSettingsPreservesOperatorPickerRows(t *testing.T) {
	seed := `{"modelPicker":{"replaceBuiltInOptions":true,"options":[{"model":"model-S","label":"My label","behavesAs":"claude-opus-5"}]}}`
	got := claudeSettingsAfterApply(t, seed, `{"env":{"ANTHROPIC_BASE_URL":"http://proxy/v1","ANTHROPIC_API_KEY":"key"}}`)
	row := claudePickerRow(t, got, "model-S")
	if row["label"] != "My label" || row["behavesAs"] != "claude-opus-5" {
		t.Fatalf("operator row was overwritten: %#v", row)
	}
	picker, _ := got["modelPicker"].(map[string]interface{})
	if picker["replaceBuiltInOptions"] != true {
		t.Fatalf("replaceBuiltInOptions was dropped: %#v", picker)
	}
	// The other three aliases still need rows.
	claudePickerRow(t, got, "model-T")
}

func TestApplyClaudeCliSettingsPersistsModelAndEffort(t *testing.T) {
	got := claudeSettingsAfterApply(t, "", `{"env":{"ANTHROPIC_BASE_URL":"http://proxy/v1","ANTHROPIC_API_KEY":"key"},"model":"model-S","reasoningEffort":"XHigh"}`)
	if got["model"] != "model-S" {
		t.Fatalf("model = %#v, want model-S", got["model"])
	}
	if got["effortLevel"] != "xhigh" {
		t.Fatalf("effortLevel = %#v, want xhigh", got["effortLevel"])
	}
	perModel, _ := got["modelSettings"].(map[string]interface{})
	entry, _ := perModel["model-s"].(map[string]interface{})
	if entry == nil || entry["effortLevel"] != "xhigh" {
		t.Fatalf("per-model effort = %#v, want xhigh keyed by lowercase id", perModel)
	}
}

// Claude Code discards a settings file whose effortLevel it cannot parse, so an
// unrecognized level must not be written at all.
func TestApplyClaudeCliSettingsRejectsUnknownEffort(t *testing.T) {
	got := claudeSettingsAfterApply(t, `{"effortLevel":"high"}`, `{"env":{"ANTHROPIC_BASE_URL":"http://proxy/v1","ANTHROPIC_API_KEY":"key"},"model":"model-S","reasoningEffort":"ultra"}`)
	if got["effortLevel"] != "high" {
		t.Fatalf("effortLevel = %#v, want the existing high to survive", got["effortLevel"])
	}
	if _, found := got["modelSettings"]; found {
		t.Fatalf("unknown effort was recorded per model: %#v", got["modelSettings"])
	}
}

// A tier slot pointed at a SOTA alias needs its capabilities declared, or Claude
// Code hides Effort for that slot.
func TestApplyClaudeCliSettingsDeclaresSotaSlotCapabilities(t *testing.T) {
	seed := `{"env":{"ANTHROPIC_DEFAULT_SONNET_MODEL":"model-T","ANTHROPIC_DEFAULT_HAIKU_MODEL":"model-T","ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-5"}}`
	got := claudeSettingsAfterApply(t, seed, `{"env":{"ANTHROPIC_BASE_URL":"http://proxy/v1","ANTHROPIC_API_KEY":"key"}}`)
	env, _ := got["env"].(map[string]interface{})
	if env["ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES"] != sotaSlotCapabilities {
		t.Fatalf("sonnet slot capabilities = %#v", env["ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES"])
	}
	// The Haiku slot already routes to a real model, so Apply must not stomp it.
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "model-T" {
		t.Fatalf("haiku slot = %#v, want the operator's model-T", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
	}
	// A model Claude Code already knows needs no override.
	if _, found := env["ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES"]; found {
		t.Fatalf("known model got a capability override: %#v", env)
	}
}
