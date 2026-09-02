package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexCatalogModels converts OmniProxy's unified model catalog into the
// fields Codex Desktop can safely consume. The desktop catalog deliberately
// receives no inferred tool or reasoning capabilities; syncCodexModelCatalog
// supplies conservative client-required defaults for those fields.
func codexCatalogModels(models []ModelInfo) []map[string]interface{} {
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
	if context > 0 {
		entry["context_window"] = context
		entry["max_context_window"] = context
	} else {
		delete(entry, "context_window")
		delete(entry, "max_context_window")
	}
	entry["input_modalities"] = modalities
}

// pruneStaleOmniProxyCatalogEntries removes models that OmniProxy previously
// published but can no longer route. The description fallback migrates files
// written before the explicit ownership marker was introduced.
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
	} {
		delete(entry, key)
	}
	return entry
}
