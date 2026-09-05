package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// assertReasoningLevels checks the Effort menu a managed entry advertises.
// Codex leaves the control unselectable when this list is empty, and each
// level needs a description or it renders without a label.
func assertReasoningLevels(t *testing.T, entry map[string]interface{}, want ...string) {
	t.Helper()
	levels, ok := entry["supported_reasoning_levels"].([]interface{})
	if !ok {
		t.Fatalf("reasoning levels missing or malformed: %#v", entry["supported_reasoning_levels"])
	}
	var efforts []string
	for _, raw := range levels {
		level, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("reasoning level is not an object: %#v", raw)
		}
		effort, _ := level["effort"].(string)
		if description, _ := level["description"].(string); description == "" {
			t.Fatalf("effort %q has no description: %#v", effort, level)
		}
		efforts = append(efforts, effort)
	}
	if !reflect.DeepEqual(efforts, want) {
		t.Fatalf("reasoning efforts = %#v, want %#v", efforts, want)
	}
}

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
	// No variant named a higher level, so this family offers the base set the
	// proxy accepts for anything it routes.
	assertReasoningLevels(t, claude, "low", "medium", "high")
	if instructions, _ := claude["base_instructions"].(string); instructions != "" {
		t.Fatal("provider model inherited another model's base instructions")
	}
	for key, want := range map[string]interface{}{
		"supports_reasoning_summaries": false,
		"support_verbosity":            false,
		// Parallel tool calls and the freeform apply_patch tool describe
		// OmniProxy's own request path, not the upstream model, so a managed
		// entry claims them. Without them the client falls back to its
		// no-capability floor and stops issuing tool calls.
		"supports_parallel_tool_calls": true,
		"apply_patch_tool_type":        "freeform",
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
	for _, key := range []string{"supports_image_detail_original", "supports_search_tool", "context_window"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("text-only model retained unverified %q: %#v", key, entry)
		}
	}
	// The seeded level list came from another model's template and must be
	// replaced by the efforts the proxy actually forwards.
	assertReasoningLevels(t, entry, "low", "medium", "high")
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
	for _, key := range []string{"supports_image_detail_original", "supports_search_tool"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("external model retained unverified %q: %#v", key, entry)
		}
	}
	assertReasoningLevels(t, entry, "low", "medium", "high")
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
	// The seed advertised a single Codex-specific level. It is replaced by the
	// proxy's own set rather than trusted or dropped entirely.
	assertReasoningLevels(t, model, "low", "medium", "high")
}

// Earlier versions seeded a new entry from Codex's "auto" template, so rows for
// Claude and the SOTA family kept Codex's own GPT-5 prompt and handed it to a
// different model on every later sync. A resync has to clear that text, not
// carry it forward.
func TestSyncCodexModelCatalogClearsInheritedCodexPrompt(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	seed := `{"models":[{"slug":"claude-opus-5","omniproxy_managed":true,` +
		`"base_instructions":"You are Codex, a coding agent based on GPT-5.",` +
		`"model_messages":{"instructions_template":"codex template","instructions_variables":{"tone":"warm"}}}]}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexModelCatalog(home, []map[string]interface{}{{
		"id": "claude-opus-5", "input_modalities": []string{"text"},
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
	entry := catalog.Models[0]
	if instructions, _ := entry["base_instructions"].(string); instructions != "" {
		t.Errorf("managed entry kept another model's instructions: %q", instructions)
	}
	messages, ok := entry["model_messages"].(map[string]interface{})
	if !ok {
		t.Fatalf("model_messages missing or malformed: %#v", entry["model_messages"])
	}
	if template, _ := messages["instructions_template"].(string); template != "" {
		t.Errorf("managed entry kept another model's instructions template: %q", template)
	}
	if vars, _ := messages["instructions_variables"].(map[string]interface{}); len(vars) != 0 {
		t.Errorf("managed entry kept another model's instruction variables: %#v", vars)
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

// Effort variants are the same upstream model at a different reasoning level,
// so they collapse. The thinking variant does not: it injects
// ThinkingModePrompt, which nothing else on the Codex path can enable.
func TestCodexModelFamilyGroupsVariants(t *testing.T) {
	for id, want := range map[string]string{
		"claude-opus-4-8":          "claude-opus-4.8",
		"claude-opus-4.8":          "claude-opus-4.8",
		"claude-opus-5-max":        "claude-opus-5",
		"claude-opus-5-xhigh":      "claude-opus-5",
		"claude-opus-4-8-thinking": "claude-opus-4.8-thinking",
		"claude-opus-5-thinking":   "claude-opus-5-thinking",
		"gp-5.6-sol-low":           "gp-5.6-sol",
		"gp-5.6-sol":               "gp-5.6-sol",
		"model-A":                  "model-a",
	} {
		if got := codexModelFamily(id); got != want {
			t.Errorf("codexModelFamily(%q) = %q, want %q", id, got, want)
		}
	}
}

// The picker should list one entry per family. This is the id set OmniProxy
// actually discovered, which filled the menu with near-identical names.
func TestCodexModelFamilyRepresentativesCollapsesDiscoveredAliases(t *testing.T) {
	discovered := []string{
		"claude-opus-5", "claude-opus-5-max", "claude-opus-5-xhigh", "claude-opus-5-thinking",
		"claude-opus-4-8", "claude-opus-4.8", "claude-opus-4-8-thinking", "claude-opus-4.8-thinking",
		"gp-5.6-sol", "gp-5.6-sol-none", "gp-5.6-sol-low", "gp-5.6-sol-max",
	}
	models := make([]ModelInfo, 0, len(discovered))
	for _, id := range discovered {
		models = append(models, ModelInfo{ModelId: id})
	}
	var ids []string
	for _, model := range codexModelFamilyRepresentatives(models, nil) {
		ids = append(ids, model.ModelId)
	}
	want := []string{
		"claude-opus-5", "claude-opus-5-thinking",
		"claude-opus-4.8", "claude-opus-4.8-thinking",
		"gp-5.6-sol",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("family representatives = %#v, want %#v", ids, want)
	}
}

// The picker can only select a slug the catalog contains, so the id named in
// config.toml must represent its family even when a bare alias exists.
func TestCodexModelFamilyRepresentativesPrefersConfiguredModel(t *testing.T) {
	models := []ModelInfo{
		{ModelId: "claude-opus-5"},
		{ModelId: "claude-opus-5-max"},
	}
	var ids []string
	for _, model := range codexModelFamilyRepresentatives(models, map[string]bool{"claude-opus-5-max": true}) {
		ids = append(ids, model.ModelId)
	}
	if !reflect.DeepEqual(ids, []string{"claude-opus-5-max"}) {
		t.Fatalf("family representatives = %#v, want the configured id", ids)
	}
}

// Each family's Effort menu comes from the variants the gateway exposes, so a
// family advertising -max offers max while a plain one stops at high. This is
// the gp-5.6-sol ladder the picker actually listed.
func TestCodexFamilyEffortsDerivesRangePerFamily(t *testing.T) {
	discovered := []string{
		"gp-5.6-sol", "gp-5.6-sol-none", "gp-5.6-sol-off", "gp-5.6-sol-auto",
		"gp-5.6-sol-minimal", "gp-5.6-sol-low", "gp-5.6-sol-medium",
		"gp-5.6-sol-high", "gp-5.6-sol-xhigh", "gp-5.6-sol-max",
		"claude-opus-5", "claude-opus-5-xhigh",
		"claude-sonnet-5",
	}
	models := make([]ModelInfo, 0, len(discovered))
	for _, id := range discovered {
		models = append(models, ModelInfo{ModelId: id})
	}
	efforts := codexFamilyEfforts(models)
	for family, want := range map[string][]string{
		// none/off/auto/minimal collapse into the family but have no Effort
		// entry: Codex's own catalog never renders those values.
		"gp-5.6-sol":      {"low", "medium", "high", "xhigh", "max"},
		"claude-opus-5":   {"low", "medium", "high", "xhigh"},
		"claude-sonnet-5": {"low", "medium", "high"},
	} {
		if got := efforts[family]; !reflect.DeepEqual(got, want) {
			t.Errorf("efforts[%q] = %#v, want %#v", family, got, want)
		}
	}
}

// The picker only lists the levels a family advertises, so the value written to
// config.toml steps down to the deepest level that family actually offers.
func TestClampCodexReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		effort    string
		available []string
		want      string
	}{
		{"max", []string{"low", "medium", "high", "xhigh", "max"}, "max"},
		{"ultra", []string{"low", "medium", "high", "xhigh", "max"}, "max"},
		{"max", []string{"low", "medium", "high"}, "high"},
		{"xhigh", []string{"low", "medium", "high"}, "high"},
		{"low", []string{"low", "medium", "high"}, "low"},
		// An unknown ladder (Codex's own models) leaves the request alone.
		{"xhigh", nil, "xhigh"},
		// Anything Codex cannot render falls back to the balanced default.
		{"minimal", []string{"low", "medium", "high"}, "medium"},
		{"", []string{"low", "medium", "high"}, "medium"},
	} {
		if got := clampCodexReasoningEffort(tc.effort, tc.available); got != tc.want {
			t.Errorf("clampCodexReasoningEffort(%q, %v) = %q, want %q", tc.effort, tc.available, got, tc.want)
		}
	}
}

// Earlier versions republished Codex's own models and marked them managed. The
// prune step must not delete them on that marker: their base instructions and
// deeper reasoning levels are Codex's, and OmniProxy cannot rebuild them.
func TestSyncCodexModelCatalogKeepsCodexBuiltinEntries(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	seed := `{"models":[
		{"slug":"gpt-5.6-sol","omniproxy_managed":true,"base_instructions":"codex prompt","supported_reasoning_levels":[{"effort":"max","description":"deep"}]},
		{"slug":"o3","omniproxy_managed":true,"base_instructions":"codex prompt"},
		{"slug":"stale-claude","omniproxy_managed":true}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncCodexModelCatalog(home, []map[string]interface{}{{
		"id": "claude-opus-5", "input_modalities": []string{"text"},
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
		slug, _ := model["slug"].(string)
		bySlug[slug] = model
	}
	sol, ok := bySlug["gpt-5.6-sol"]
	if !ok {
		t.Fatalf("Codex builtin was pruned: %#v", catalog.Models)
	}
	if sol["base_instructions"] != "codex prompt" {
		t.Fatalf("Codex builtin lost its instructions: %#v", sol)
	}
	if levels, _ := sol["supported_reasoning_levels"].([]interface{}); len(levels) != 1 {
		t.Fatalf("Codex builtin lost its reasoning levels: %#v", sol)
	}
	// Ownership goes back to Codex so later syncs leave the entry alone.
	if _, managed := sol["omniproxy_managed"]; managed {
		t.Fatalf("Codex builtin is still marked OmniProxy-managed: %#v", sol)
	}
	if _, ok := bySlug["o3"]; !ok {
		t.Fatalf("o-series builtin was pruned: %#v", catalog.Models)
	}
	// A managed non-Codex model that no longer routes is still removed.
	if _, ok := bySlug["stale-claude"]; ok {
		t.Fatalf("stale managed model survived: %#v", bySlug["stale-claude"])
	}
}

// The discovery cache also holds the media generators the proxy routes through
// its image, video and audio endpoints. Codex's picker drives a text
// conversation, so publishing those would crowd it with unusable rows.
func TestCodexDesktopModelsExcludesGenerativeModels(t *testing.T) {
	media := func(id, output string) ModelInfo {
		return ModelInfo{ModelId: id, OutputTypes: []string{output}, Modalities: []string{output}}
	}
	h := &Handler{cachedModels: []ModelInfo{
		{ModelId: "claude-opus-5"},
		// A vision model still belongs in the picker: it reads images, not writes.
		{ModelId: "model-A", InputTypes: []string{"text", "image"}, OutputTypes: []string{"text"}},
		media("midjourney_8_2", "image"),
		media("kling_video_3_0", "video"),
		media("eleven_v3", "audio"),
		media("suno-v5.5", "music"),
	}}
	models, _ := h.codexDesktopModels("claude-opus-5")
	published := make(map[string]bool, len(models))
	for _, model := range models {
		published[model.ModelId] = true
	}
	for _, id := range []string{"claude-opus-5", "model-A"} {
		if !published[id] {
			t.Errorf("chat model %q missing from picker", id)
		}
	}
	for _, id := range []string{"midjourney_8_2", "kling_video_3_0", "eleven_v3", "suno-v5.5"} {
		if published[id] {
			t.Errorf("generative model %q was published to the picker", id)
		}
	}
}

// A gateway may list a model without token metadata. The routing policy fills
// only documented model families; aliases without upstream limits must remain
// unset rather than inheriting an unrelated context window.
func TestCodexDesktopModelsFillsOnlyDocumentedTokenLimits(t *testing.T) {
	h := &Handler{cachedModels: []ModelInfo{
		{ModelId: "claude-opus-5"},
		{ModelId: "model-A"},
		{ModelId: "mystery-model-9"},
	}}
	models, efforts := h.codexDesktopModels("claude-opus-5")

	limits := make(map[string]int, len(models))
	for _, model := range models {
		if model.TokenLimits != nil {
			limits[model.ModelId] = model.TokenLimits.MaxInputTokens
		}
	}
	if limits["claude-opus-5"] != 1_000_000 {
		t.Errorf("claude-opus-5 max input tokens = %d, want 1000000", limits["claude-opus-5"])
	}
	for _, id := range []string{"model-A", "mystery-model-9"} {
		if got, ok := limits[id]; ok {
			t.Errorf("%s was given invented token limits: %d", id, got)
		}
	}

	// The window has to survive the conversion into catalog entries, which is
	// what syncCodexModelCatalog turns into context_window.
	for _, entry := range codexCatalogModels(models, efforts) {
		if entry["id"] != "claude-opus-5" {
			continue
		}
		tokens, ok := entry["token_limits"].(map[string]interface{})
		if !ok || tokens["maxInputTokens"] != 1_000_000 {
			t.Fatalf("catalog entry lost the token limits: %#v", entry)
		}
	}
}
