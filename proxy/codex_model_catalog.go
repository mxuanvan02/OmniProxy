package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// codexBuiltinModelPattern matches the id namespaces Codex already ships in its
// own catalog: the GPT families, the o-series, and the Codex-specific models.
// OmniProxy has nothing to add there, and republishing them would replace
// Codex's richer metadata with our own conservative entries.
var codexBuiltinModelPattern = regexp.MustCompile(`^(?:gpt(?:[-.]|$)|o\d+(?:[-.]|$)|codex(?:[-.]|$))`)

// codexEffortSuffixes are reasoning levels some gateways expose as separate
// model ids (claude-opus-5-max, gp-5.6-sol-low). OmniProxy forwards
// reasoning.effort verbatim, so these are the same upstream model at a
// different effort and belong behind the picker's Effort control.
var codexEffortSuffixes = []string{
	"none", "off", "auto", "minimal", "low", "medium", "high", "xhigh", "max", "ultra",
}

// codexRenderableEfforts are the effort values Codex's own catalog uses, in the
// order it lists them. Suffixes outside this set (none, off, auto, minimal)
// still collapse into their family, but they have no Effort entry to select.
var codexRenderableEfforts = []string{"low", "medium", "high", "xhigh", "max", "ultra"}

var codexEffortDescriptions = map[string]string{
	"low":    "Fast responses with lighter reasoning",
	"medium": "Balances speed and reasoning depth for everyday tasks",
	"high":   "Greater reasoning depth for complex problems",
	"xhigh":  "Extra high reasoning depth for complex problems",
	"max":    "Maximum reasoning depth for the hardest problems",
	"ultra":  "Maximum reasoning with automatic task delegation",
}

// codexBaseEfforts are the levels OmniProxy accepts for any model it routes,
// regardless of what the gateway names in its ids. Families that advertise
// deeper levels get them added on top.
var codexBaseEfforts = []string{"low", "medium", "high"}

// clampCodexReasoningEffort resolves the effort to write into config.toml. Any
// level Codex can render is accepted, but when the model's family advertises a
// narrower ladder the value steps down to the closest level that family does
// offer, so the configured default is always one the picker can display for
// that model. An empty ladder means the range is unknown, as it is for Codex's
// own models, and the requested level stands.
func clampCodexReasoningEffort(effort string, available []string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	index := -1
	for i, candidate := range codexRenderableEfforts {
		if effort == candidate {
			index = i
			break
		}
	}
	if index < 0 {
		return "medium"
	}
	if len(available) == 0 {
		return effort
	}
	offered := make(map[string]bool, len(available))
	for _, level := range available {
		offered[strings.ToLower(strings.TrimSpace(level))] = true
	}
	for i := index; i >= 0; i-- {
		if offered[codexRenderableEfforts[i]] {
			return codexRenderableEfforts[i]
		}
	}
	return "medium"
}

// codexGenerativeOutputTypes are the non-text output modalities a discovered
// model can declare. Codex's picker drives a text conversation, so an image,
// video, or audio generator has no place in it even though the proxy can route
// it through the image and media endpoints.
var codexGenerativeOutputTypes = []string{"image", "video", "audio", "music", "speech"}

// isCodexChatModel reports whether a discovered model belongs in the picker.
// Only OutputTypes is consulted. Modalities is a flattened view of the
// provider's modality object, so it mixes input keys in with output ones and
// would read a vision chat model as an image generator.
func isCodexChatModel(model ModelInfo) bool {
	for _, value := range model.OutputTypes {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || strings.Contains(value, "text") {
			continue
		}
		for _, kind := range codexGenerativeOutputTypes {
			if strings.Contains(value, kind) {
				return false
			}
		}
	}
	return true
}

func isCodexBuiltinModel(id string) bool {
	return codexBuiltinModelPattern.MatchString(strings.ToLower(strings.TrimSpace(id)))
}

// codexEffortSuffixOf reports the reasoning level a gateway encoded in a model
// id, so gp-5.6-sol-max yields "max". An id that names no level yields "".
func codexEffortSuffixOf(id string) string {
	base, _ := ParseModelAndThinking(strings.ToLower(strings.TrimSpace(id)), "-thinking")
	for _, suffix := range codexEffortSuffixes {
		if trimmed := strings.TrimSuffix(base, "-"+suffix); trimmed != base && trimmed != "" {
			return suffix
		}
	}
	return ""
}

// codexFamilyEfforts derives the Effort menu for each family from the ids the
// gateway actually exposes. A family whose variants stop at high gets no xhigh
// entry, and one advertising -max gets it, so the menu mirrors the upstream
// instead of a single hardcoded range. The base levels are always included
// because OmniProxy accepts them for every model it routes.
func codexFamilyEfforts(models []ModelInfo) map[string][]string {
	seen := make(map[string]map[string]bool, len(models))
	for _, model := range models {
		id := strings.ToLower(strings.TrimSpace(model.ModelId))
		if id == "" {
			continue
		}
		family := codexModelFamily(id)
		if seen[family] == nil {
			seen[family] = make(map[string]bool, len(codexRenderableEfforts))
			for _, effort := range codexBaseEfforts {
				seen[family][effort] = true
			}
		}
		if effort := codexEffortSuffixOf(id); effort != "" {
			seen[family][effort] = true
		}
	}
	out := make(map[string][]string, len(seen))
	for family, efforts := range seen {
		ordered := make([]string, 0, len(efforts))
		for _, effort := range codexRenderableEfforts {
			if efforts[effort] {
				ordered = append(ordered, effort)
			}
		}
		out[family] = ordered
	}
	return out
}

// codexModelFamily reduces a model id to the family the picker should show. An
// effort suffix is dropped because effort is a UI control the proxy forwards
// verbatim, and Anthropic's dashed version form is normalized so
// claude-opus-4-8 and claude-opus-4.8 name one family.
//
// The thinking suffix is deliberately kept: it injects ThinkingModePrompt into
// the system prompt (see OpenAIToKiro), and on the Codex path nothing else can
// turn that on, so a thinking variant is a genuinely different model.
func codexModelFamily(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return ""
	}
	base, thinking := ParseModelAndThinking(id, "-thinking")
	base = strings.ToLower(strings.TrimSpace(base))
	for _, suffix := range codexEffortSuffixes {
		trimmed := strings.TrimSuffix(base, "-"+suffix)
		if trimmed != base && trimmed != "" {
			base = trimmed
			break
		}
	}
	// Re-normalize: stripping an effort suffix can expose a dashed version
	// form, as in claude-opus-4-8-max.
	family, _ := ParseModelAndThinking(base, "-thinking")
	family = strings.ToLower(strings.TrimSpace(family))
	if family == "" {
		family = base
	}
	if family == "" {
		return id
	}
	if thinking {
		return family + "-thinking"
	}
	return family
}

// codexModelFamilyRepresentatives keeps one model per family. A configured
// model wins, because config.toml names that exact id and the picker can only
// show a slug the catalog contains. Otherwise the bare family alias wins, and
// failing that the first listed member represents the family.
func codexModelFamilyRepresentatives(models []ModelInfo, preferred map[string]bool) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	indexByFamily := make(map[string]int, len(models))
	chosen := make(map[string]bool, len(models))
	for _, model := range models {
		id := strings.ToLower(strings.TrimSpace(model.ModelId))
		if id == "" {
			continue
		}
		family := codexModelFamily(id)
		index, seen := indexByFamily[family]
		if !seen {
			indexByFamily[family] = len(out)
			chosen[family] = preferred[id]
			out = append(out, model)
			continue
		}
		if chosen[family] {
			continue
		}
		if preferred[id] {
			out[index] = model
			chosen[family] = true
			continue
		}
		if id == family {
			out[index] = model
		}
	}
	return out
}

// codexDesktopModels returns the models OmniProxy adds to the Codex picker:
// one entry per discovered non-Codex family (Claude and the other frontier
// families), plus whatever is currently configured to route. Publishing the
// whole unified catalog filled the menu with every cached provider id,
// including per-effort and per-thinking aliases of one upstream model.
//
// The second return value maps each family to the Effort menu derived from the
// variants that collapsed into it, so the levels a gateway advertises survive
// as a UI control instead of as extra rows.
func (h *Handler) codexDesktopModels(selected ...string) ([]ModelInfo, map[string][]string) {
	h.modelsCacheMu.RLock()
	cached := append([]ModelInfo(nil), h.cachedModels...)
	h.modelsCacheMu.RUnlock()

	known := make(map[string]ModelInfo, len(cached))
	for _, model := range cached {
		known[strings.ToLower(strings.TrimSpace(model.ModelId))] = model
	}

	preferred := make(map[string]bool, len(selected))
	configured := make([]string, 0, len(selected))
	for _, raw := range selected {
		id := openClawModelID(strings.TrimSpace(raw))
		// A configured Codex model still routes through the proxy via the
		// top-level model_provider, so Codex's own catalog entry serves it.
		if id == "" || isCodexBuiltinModel(id) {
			continue
		}
		preferred[strings.ToLower(id)] = true
		configured = append(configured, id)
	}

	candidates := make([]ModelInfo, 0, len(cached))
	for _, model := range cached {
		if isCodexBuiltinModel(model.ModelId) || !isCodexChatModel(model) {
			continue
		}
		candidates = append(candidates, model)
	}
	models := codexModelFamilyRepresentatives(candidates, preferred)

	for _, id := range configured {
		model, ok := known[strings.ToLower(id)]
		if !ok {
			model = ModelInfo{ModelId: id, ModelName: id}
		}
		models = mergeUniqueModels(models, []ModelInfo{model})
	}
	// An entry without token metadata is written without context_window, and the
	// client then applies its own default, so a 1M model gets truncated
	// mid-task. Fill the gap for every published model rather than only the
	// configured one: the picker can select any row it shows.
	for i := range models {
		if models[i].TokenLimits != nil && models[i].TokenLimits.MaxInputTokens > 0 {
			continue
		}
		input, output, found := policyModelLimits(models[i].ModelId)
		if !found {
			continue
		}
		models[i].TokenLimits = &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: input, MaxOutputTokens: output}
	}
	// Derive efforts from every discovered variant, not just the published
	// representatives: the variants are what name the levels.
	return models, codexFamilyEfforts(append(candidates, models...))
}

// codexCatalogModels converts OmniProxy's unified model catalog into the
// fields Codex Desktop can safely consume. The desktop catalog deliberately
// receives no inferred tool or reasoning capabilities; syncCodexModelCatalog
// supplies conservative client-required defaults for those fields.
func codexCatalogModels(models []ModelInfo, efforts map[string][]string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ModelId)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(model.ModelName)
		if name == "" {
			name = id
		}
		entry := map[string]interface{}{
			"id":               id,
			"name":             name,
			"description":      model.Description,
			"input_modalities": model.InputTypes,
		}
		if levels := efforts[codexModelFamily(id)]; len(levels) > 0 {
			entry["reasoning_efforts"] = levels
		}
		if model.TokenLimits != nil {
			entry["token_limits"] = map[string]interface{}{
				"maxInputTokens": model.TokenLimits.MaxInputTokens,
			}
		}
		out = append(out, entry)
	}
	return out
}

// syncCodexModelCatalog updates the local catalog used by Codex Desktop. The
// desktop picker reads this file instead of requesting /v1/models on demand,
// so configuring a custom OpenAI-compatible provider alone is not sufficient.
// Existing entries are retained because they contain Codex client metadata not
// represented by the OpenAI model-list response.
func syncCodexModelCatalog(homeDir string, models []map[string]interface{}) error {
	path := filepath.Join(homeDir, ".codex", "model-catalog.json")
	root := map[string]interface{}{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("decode Codex model catalog: %w", err)
		}
	}

	existing, _ := root["models"].([]interface{})
	bySlug := make(map[string]int, len(existing))
	for index, raw := range existing {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		slug, _ := entry["slug"].(string)
		if slug != "" {
			bySlug[strings.ToLower(slug)] = index
		}
	}

	active := make(map[string]bool, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		active[key] = true
		index, exists := bySlug[key]
		var current map[string]interface{}
		if exists {
			current, _ = existing[index].(map[string]interface{})
		}
		entry := cloneCatalogEntry(current, nil)
		applyCodexCatalogMetadata(entry, id, model)
		applyRequiredCodexCatalogFields(entry)
		if exists {
			existing[index] = entry
		} else {
			existing = append(existing, entry)
			index = len(existing) - 1
		}
		bySlug[key] = index
	}
	existing = pruneStaleOmniProxyCatalogEntries(existing, active)

	root["models"] = existing
	root["fetched_at"] = time.Now().UTC().Format(time.RFC3339)
	root["client_version"] = "omniproxy"
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Codex model catalog: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".model-catalog-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func cloneCatalogEntry(entry, fallback map[string]interface{}) map[string]interface{} {
	if entry == nil {
		entry = fallback
	}
	if entry == nil {
		return map[string]interface{}{}
	}
	raw, _ := json.Marshal(entry)
	clone := map[string]interface{}{}
	_ = json.Unmarshal(raw, &clone)
	return clone
}

func applyCodexCatalogMetadata(entry map[string]interface{}, id string, model map[string]interface{}) {
	// Codex Desktop's bundled entries have extra, model-specific tool metadata.
	// Preserve that only for the known Codex subscription entries. Provider-discovered
	// models must not inherit unverified image, search, or reasoning support
	// from the "auto" template. Do not trust owned_by here: external gateways
	// can label a non-Codex model as openai-codex.
	if !isCodexSubscriptionModel(id) {
		entry = clearUnverifiedCodexCapabilities(entry)
	}
	name, _ := model["name"].(string)
	if strings.TrimSpace(name) == "" {
		name = id
	}
	description, _ := model["description"].(string)
	if strings.TrimSpace(description) == "" {
		description = name + " via OmniProxy."
	}
	context := 0
	if limits, ok := model["token_limits"].(map[string]interface{}); ok {
		if value, ok := limits["maxInputTokens"].(int); ok && value > 0 {
			context = value
		} else if value, ok := limits["maxInputTokens"].(float64); ok && value > 0 {
			context = int(value)
		}
	}
	modalities := []interface{}{"text"}
	if raw, ok := model["input_modalities"].([]string); ok && len(raw) > 0 {
		modalities = make([]interface{}, len(raw))
		for i, value := range raw {
			modalities[i] = value
		}
	}
	entry["slug"] = id
	entry["display_name"] = name
	entry["description"] = description
	entry["visibility"] = "list"
	entry["supported_in_api"] = true
	entry["omniproxy_managed"] = true
	if !isCodexSubscriptionModel(id) {
		// An empty list leaves Effort unselectable, so every managed entry
		// carries the levels its family actually offers.
		entry["supported_reasoning_levels"] = codexReasoningLevels(catalogModelEfforts(model))
		entry["default_reasoning_level"] = "medium"
		// These two are properties of the request path, not of the upstream
		// model: every managed entry is routed through OmniProxy's own
		// translator, which emits parallel tool calls and the freeform
		// apply_patch tool for any model it serves. Left absent, the client
		// applies its no-capability floor and stops issuing tool calls, which
		// looks like the model announcing an edit and then not making it.
		entry["supports_parallel_tool_calls"] = true
		entry["apply_patch_tool_type"] = "freeform"
	}
	if context > 0 {
		entry["context_window"] = context
		entry["max_context_window"] = context
	} else {
		delete(entry, "context_window")
		delete(entry, "max_context_window")
	}
	entry["input_modalities"] = modalities
}

// catalogModelEfforts reads the effort list codexCatalogModels attached to a
// model. Callers that build entries by hand get the base levels OmniProxy
// accepts for anything it routes.
func catalogModelEfforts(model map[string]interface{}) []string {
	switch levels := model["reasoning_efforts"].(type) {
	case []string:
		if len(levels) > 0 {
			return levels
		}
	case []interface{}:
		out := make([]string, 0, len(levels))
		for _, raw := range levels {
			if effort, ok := raw.(string); ok && effort != "" {
				out = append(out, effort)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return codexBaseEfforts
}

// codexReasoningLevels renders an effort list in the shape Codex's catalog
// uses. Unknown values are skipped rather than shown without a description.
func codexReasoningLevels(efforts []string) []interface{} {
	out := make([]interface{}, 0, len(efforts))
	for _, effort := range efforts {
		description, ok := codexEffortDescriptions[effort]
		if !ok {
			continue
		}
		out = append(out, map[string]interface{}{"effort": effort, "description": description})
	}
	return out
}

// pruneStaleOmniProxyCatalogEntries removes models that OmniProxy previously
// published but can no longer route. The description fallback migrates files
// written before the explicit ownership marker was introduced.
//
// Codex's own model namespace is never pruned. Earlier versions republished
// those entries and marked them managed, so pruning on that marker alone would
// delete Codex's bundled models along with their base instructions and deeper
// reasoning levels, which OmniProxy cannot reconstruct. The marker is cleared
// instead, handing ownership back to Codex.
func pruneStaleOmniProxyCatalogEntries(entries []interface{}, active map[string]bool) []interface{} {
	out := make([]interface{}, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			out = append(out, raw)
			continue
		}
		slug, _ := entry["slug"].(string)
		key := strings.ToLower(strings.TrimSpace(slug))
		if key != "" && isCodexBuiltinModel(key) {
			delete(entry, "omniproxy_managed")
			out = append(out, entry)
			continue
		}
		if key != "" && !active[key] && isOmniProxyManagedCatalogEntry(entry) {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func isOmniProxyManagedCatalogEntry(entry map[string]interface{}) bool {
	if managed, _ := entry["omniproxy_managed"].(bool); managed {
		return true
	}
	description, _ := entry["description"].(string)
	return strings.HasSuffix(strings.TrimSpace(description), " via OmniProxy.")
}

func isCodexSubscriptionModel(id string) bool {
	for _, model := range codexSubscriptionModels() {
		if strings.EqualFold(strings.TrimSpace(model.ModelId), strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

// requiredCodexCatalogFields are fields the Codex client refuses to decode a
// catalog without. clearUnverifiedCodexCapabilities deletes several of them to
// avoid advertising capabilities we cannot verify, which leaves the file
// unparseable on clients that treat them as mandatory. Fill each one with the
// most conservative value that still decodes: a capability flag becomes false,
// a list becomes empty, and prompt text stays blank so no model inherits
// another model's instructions. These are floors, not claims.
//
// shell_type and truncation_policy are client-side plumbing rather than model
// capability claims, so they take the ordinary client defaults.
func requiredCodexCatalogFields() map[string]func() interface{} {
	return map[string]func() interface{}{
		"supported_reasoning_levels":   func() interface{} { return []interface{}{} },
		"experimental_supported_tools": func() interface{} { return []interface{}{} },
		"supports_reasoning_summaries": func() interface{} { return false },
		"supports_parallel_tool_calls": func() interface{} { return false },
		"support_verbosity":            func() interface{} { return false },
		"priority":                     func() interface{} { return 0 },
		"base_instructions":            func() interface{} { return "" },
		"shell_type":                   func() interface{} { return "shell_command" },
		"truncation_policy": func() interface{} {
			return map[string]interface{}{"limit": 10000, "mode": "tokens"}
		},
		"model_messages": func() interface{} {
			return map[string]interface{}{
				"instructions_template":  "",
				"instructions_variables": map[string]interface{}{},
			}
		},
	}
}

// applyRequiredCodexCatalogFields adds only the mandatory fields that are
// absent. Entries that already carry a real value keep it, so Codex
// subscription models retain their genuine metadata.
func applyRequiredCodexCatalogFields(entry map[string]interface{}) map[string]interface{} {
	for key, value := range requiredCodexCatalogFields() {
		if _, ok := entry[key]; !ok {
			entry[key] = value()
		}
	}
	return entry
}

func clearUnverifiedCodexCapabilities(entry map[string]interface{}) map[string]interface{} {
	for _, key := range []string{
		"additional_speed_tiers", "apply_patch_tool_type", "default_reasoning_level",
		"default_reasoning_summary", "default_verbosity", "effective_context_window_percent",
		"experimental_supported_tools", "shell_type", "support_verbosity",
		"supported_reasoning_levels", "supports_image_detail_original",
		"supports_parallel_tool_calls", "supports_reasoning_summaries",
		"supports_search_tool", "truncation_policy", "web_search_tool_type",
		// Prompt text is model-specific. Earlier versions seeded new entries
		// from Codex's "auto" template, so a Claude or SOTA row could keep
		// Codex's own GPT-5 instructions and hand them to a different model on
		// every later sync. Dropping both here lets
		// applyRequiredCodexCatalogFields restore blank floors instead.
		"base_instructions", "model_messages",
	} {
		delete(entry, key)
	}
	return entry
}
