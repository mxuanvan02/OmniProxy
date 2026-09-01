package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"omniproxy/auth"
	"omniproxy/config"
	"omniproxy/logger"
	"omniproxy/pool"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Proactive refresh buffer: refresh token when within 5 minutes of expiry.
// REFRESH_LEAD_MS[kiro] = 5 * 60 * 1000.
// This ensures tokens are refreshed BEFORE they expire, preventing 403 errors.
const tokenRefreshSkewSeconds int64 = 300

var (
	cliToolConfigured   = map[string]bool{}
	cliToolConfiguredMu sync.RWMutex
)

type CliToolSettings struct {
	BaseURL       string            `json:"baseUrl,omitempty"`
	APIKey        string            `json:"apiKey,omitempty"`
	Model         string            `json:"model,omitempty"`
	Models        []string          `json:"models,omitempty"`
	ActiveModel   string            `json:"activeModel,omitempty"`
	SubagentModel string            `json:"subagentModel,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	AgentModels   map[string]string `json:"agentModels,omitempty"`
	Config        string            `json:"config,omitempty"`
}

const (
	defaultAgentModel = "gpt-5.6-luna"
	deepResearchModel = "gpt-5.6-sol"
	imageAgentModel   = "gpt-5.6-luna"
)

// policyModelLimits contains fallback limits for configured models whose
// upstream catalog entries may be absent or incomplete. A catalog entry is
// authoritative when it has token metadata; these values fill only gaps.
func policyModelLimits(model string) (int, int, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		model = strings.TrimSpace(model[idx+1:])
	}
	model, _ = ParseModelAndThinking(model, "-thinking")
	switch model {
	case defaultAgentModel:
		return 272000, 128000, true
	case deepResearchModel:
		return 272000, 128000, true
	case "claude-opus-5", "claude-sonnet-5", "claude-haiku-5":
		return 1_000_000, 128_000, true
	default:
		return 0, 0, false
	}
}

func (h *Handler) omniProxyModelCatalog(extra ...string) []ModelInfo {
	h.modelsCacheMu.RLock()
	cached := append([]ModelInfo(nil), h.cachedModels...)
	h.modelsCacheMu.RUnlock()
	// Codex's static catalog is the authority for its model metadata. Keep it
	// first so an old discovery-cache record cannot publish stale limits.
	models := mergeUniqueModels(codexSubscriptionModels(), cached)
	for _, model := range extra {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		models = mergeUniqueModels(models, []ModelInfo{{ModelId: openClawModelID(model), ModelName: openClawModelID(model)}})
	}
	return models
}

// openClawModelRef returns the model reference OpenClaw expects in agents and
// defaults. Bare model IDs belong to the OmniProxy provider; explicit provider
// references are preserved so selecting a model from another configured
// provider does not silently route it through the wrong namespace.
func openClawModelRef(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || strings.Contains(model, "/") {
		return model
	}
	return "omniproxy/" + model
}

func openClawModelID(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		prefix := strings.ToLower(strings.TrimSpace(model[:idx]))
		switch prefix {
		case "omniproxy", "kiro", "kiro-proxy", "superkiro", "codex", "openai-codex":
			return strings.TrimSpace(model[idx+1:])
		}
	}
	return model
}

func mergeOpenClawProviderModel(provider map[string]interface{}, model string, contextWindow, maxTokens int) {
	model = openClawModelID(model)
	if model == "" {
		return
	}

	models, _ := provider["models"].([]interface{})
	for _, raw := range models {
		if entry, ok := raw.(map[string]interface{}); ok {
			if id, _ := entry["id"].(string); id == model {
				if contextWindow > 0 {
					entry["contextWindow"] = contextWindow
				}
				if maxTokens > 0 {
					entry["maxTokens"] = maxTokens
				}
				return
			}
		}
	}
	entry := map[string]interface{}{"id": model, "name": model}
	if contextWindow > 0 {
		entry["contextWindow"] = contextWindow
	}
	if maxTokens > 0 {
		entry["maxTokens"] = maxTokens
	}
	models = append(models, entry)
	provider["models"] = models
}

func mergeOpenClawProviderInfo(provider map[string]interface{}, info ModelInfo) {
	id := openClawModelID(info.ModelId)
	if id == "" {
		return
	}
	contextWindow, maxTokens, _ := modelInfoTokenLimits(info)
	mergeOpenClawProviderModel(provider, id, contextWindow, maxTokens)
	models, _ := provider["models"].([]interface{})
	for _, raw := range models {
		entry, ok := raw.(map[string]interface{})
		if !ok || entry["id"] != id {
			continue
		}
		if info.ModelName != "" {
			entry["name"] = info.ModelName
		}
		// OpenClaw's provider-model schema does not permit descriptions. Remove
		// stale entries written by older OmniProxy versions as well, otherwise a
		// config refresh can prevent the gateway from starting.
		delete(entry, "description")
		if len(info.InputTypes) > 0 {
			entry["input"] = info.InputTypes
		}
		return
	}
}

func mergeOpenClawProviderCatalog(provider map[string]interface{}, catalog []ModelInfo) {
	// OpenClaw validates every provider-model entry against a strict schema.
	// Remove descriptions from the whole existing catalog, including entries
	// that are no longer returned by OmniProxy, before merging fresh metadata.
	if models, ok := provider["models"].([]interface{}); ok {
		for _, raw := range models {
			if entry, ok := raw.(map[string]interface{}); ok {
				delete(entry, "description")
			}
		}
	}
	for _, info := range catalog {
		mergeOpenClawProviderInfo(provider, info)
	}
}

func mergeOpenClawAgents(current map[string]interface{}, primaryModel string, agentModels map[string]string) {
	defaults, _ := current["defaults"].(map[string]interface{})
	if defaults == nil {
		defaults = map[string]interface{}{}
		current["defaults"] = defaults
	}
	setOpenClawPrimary(defaults, "model", openClawModelRef(primaryModel))
	setOpenClawPrimary(defaults, "imageGenerationModel", openClawModelRef(imageAgentModel))
	setOpenClawPrimary(defaults, "imageModel", openClawModelRef(imageAgentModel))

	allowed, _ := defaults["models"].(map[string]interface{})
	if allowed == nil {
		allowed = map[string]interface{}{}
		defaults["models"] = allowed
	}
	addOpenClawAllowedModel(allowed, openClawModelRef(primaryModel), primaryModel)
	addOpenClawAllowedModel(allowed, openClawModelRef(imageAgentModel), imageAgentModel)

	list, _ := current["list"].([]interface{})
	byID := make(map[string]map[string]interface{}, len(list))
	for _, raw := range list {
		if agent, ok := raw.(map[string]interface{}); ok {
			if id, _ := agent["id"].(string); id != "" {
				byID[id] = agent
			}
		}
	}
	for id, model := range agentModels {
		id = strings.TrimSpace(id)
		model = strings.TrimSpace(model)
		if id == "" || model == "" {
			continue
		}
		if isResearchAgentID(id) {
			model = deepResearchModel
		}
		ref := openClawModelRef(model)
		if agent, ok := byID[id]; ok {
			agent["model"] = ref
		} else {
			agent := map[string]interface{}{"id": id, "model": ref}
			list = append(list, agent)
			byID[id] = agent
		}
		addOpenClawAllowedModel(allowed, ref, model)
	}
	for _, raw := range list {
		agent, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := agent["id"].(string)
		if !isResearchAgentID(id) {
			continue
		}
		agent["model"] = openClawModelRef(deepResearchModel)
		addOpenClawAllowedModel(allowed, openClawModelRef(deepResearchModel), deepResearchModel)
	}
	current["list"] = list
}

func setOpenClawPrimary(defaults map[string]interface{}, key, ref string) {
	if ref == "" {
		return
	}
	model, _ := defaults[key].(map[string]interface{})
	if model == nil {
		model = map[string]interface{}{}
		defaults[key] = model
	}
	model["primary"] = ref
}

func addOpenClawAllowedModel(allowed map[string]interface{}, ref, model string) {
	input, output, ok := policyModelLimits(model)
	if !ok {
		return
	}
	addOpenClawAllowedModelLimits(allowed, ref, input, output)
}

func addOpenClawAllowedModelLimits(allowed map[string]interface{}, ref string, input, output int) {
	if ref == "" {
		return
	}
	entry, _ := allowed[ref].(map[string]interface{})
	if entry == nil {
		entry = map[string]interface{}{}
		allowed[ref] = entry
	}
	params, _ := entry["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
		entry["params"] = params
	}
	if output > 0 {
		params["maxTokens"] = output
	}
	_ = input // OpenClaw stores the input window in the provider catalog.
}

func mergeOpenClawAllowedCatalog(allowed map[string]interface{}, catalog []ModelInfo) {
	for _, info := range catalog {
		id := openClawModelID(info.ModelId)
		if id == "" {
			continue
		}
		input, output, ok := modelInfoTokenLimits(info)
		// Keep every catalog model selectable. Token metadata is optional, so
		// an entry without limits must still be present in the picker.
		if !ok {
			input, output = 0, 0
		}
		addOpenClawAllowedModelLimits(allowed, openClawModelRef(id), input, output)
	}
}

func isResearchAgentID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.Contains(id, "research") || strings.Contains(id, "scientific")
}

var (
	cliToolSettings   = map[string]*CliToolSettings{}
	cliToolSettingsMu sync.RWMutex
)

func setCliToolSettings(toolID string, s *CliToolSettings) {
	cliToolSettingsMu.Lock()
	cliToolSettings[toolID] = s
	cliToolSettingsMu.Unlock()
}

func getCliToolSettings(toolID string) *CliToolSettings {
	cliToolSettingsMu.RLock()
	defer cliToolSettingsMu.RUnlock()
	return cliToolSettings[toolID]
}

func delCliToolSettings(toolID string) {
	cliToolSettingsMu.Lock()
	delete(cliToolSettings, toolID)
	cliToolSettingsMu.Unlock()
}

func markCliToolConfigured(toolID string, configured bool) {
	cliToolConfiguredMu.Lock()
	if configured {
		cliToolConfigured[toolID] = true
	} else {
		delete(cliToolConfigured, toolID)
	}
	cliToolConfiguredMu.Unlock()
}

func getCliToolConfigured() map[string]bool {
	cliToolConfiguredMu.RLock()
	defer cliToolConfiguredMu.RUnlock()
	out := make(map[string]bool, len(cliToolConfigured))
	for k, v := range cliToolConfigured {
		out[k] = v
	}
	return out
}

// isOmniProxyActiveProvider checks whether the ACTIVE (uncommented)
// model_provider in a TOML config file is set to "omniproxy" (new name)
// or "superkiro" (legacy name for backward compatibility).
// A line starting with # is a comment and is ignored.
func isOmniProxyActiveProvider(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		// Strip inline comment (everything after # preceded by space)
		if idx := strings.IndexAny(trimmed, "#"); idx > 0 {
			before := strings.TrimSpace(trimmed[:idx])
			if before != "" {
				trimmed = before
			}
		}
		if !strings.Contains(strings.ToLower(trimmed), "model_provider") {
			continue
		}
		if strings.Contains(trimmed, `"omniproxy"`) || strings.Contains(trimmed, `"superkiro"`) {
			return true
		}
	}
	return false
}

func upsertYAMLProviderBlock(raw, providerName, providerBlock string) string {
	lines := strings.Split(raw, "\n")
	providersIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "providers:" && len(line)-len(strings.TrimLeft(line, " ")) == 0 {
			providersIdx = i
			break
		}
	}
	if providersIdx == -1 {
		if strings.TrimSpace(raw) == "" {
			return "providers:\n" + providerBlock
		}
		return strings.TrimRight(raw, "\n") + "\nproviders:\n" + providerBlock
	}

	start := -1
	for i := providersIdx + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if trimmed != "" && indent == 0 {
			break
		}
		if indent == 2 && trimmed == providerName+":" {
			start = i
			break
		}
	}
	if start != -1 {
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			line := lines[i]
			trimmed := strings.TrimSpace(line)
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if trimmed != "" && indent <= 2 {
				end = i
				break
			}
		}
		lines = append(lines[:start], lines[end:]...)
	}

	insert := strings.Split(strings.TrimRight(providerBlock, "\n"), "\n")
	out := append([]string{}, lines[:providersIdx+1]...)
	out = append(out, insert...)
	out = append(out, lines[providersIdx+1:]...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

// upsertYAMLModelSection updates the top-level `model:` section in a Hermes
// config.yaml so that the default model, provider, base_url, api_key, and
// api_mode always point to OmniProxy. If the section doesn't exist, it is
// inserted at the top of the file. Existing sub-keys (context_length,
// max_tokens, etc.) are preserved unless overridden.
func upsertYAMLModelSection(raw, model, provider, baseURL, apiKey string, limits ...int) string {
	lines := strings.Split(raw, "\n")

	// Locate the top-level `model:` section.
	modelIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "model:" && len(line)-len(strings.TrimLeft(line, " ")) == 0 {
			modelIdx = i
			break
		}
	}

	// Desired key-values (order matters for readability).
	desired := map[string]string{
		"default":  model,
		"provider": provider,
		"base_url": baseURL,
		"api_key":  apiKey,
		"api_mode": "openai",
	}
	desiredOrder := []string{"default", "provider", "base_url", "api_key", "api_mode"}
	if len(limits) > 0 && limits[0] > 0 {
		desired["context_length"] = strconv.Itoa(limits[0])
		desiredOrder = append(desiredOrder, "context_length")
	}
	if len(limits) > 1 && limits[1] > 0 {
		desired["max_tokens"] = strconv.Itoa(limits[1])
		desiredOrder = append(desiredOrder, "max_tokens")
	}

	if modelIdx == -1 {
		// No model: section — insert one at the very top.
		section := []string{"model:"}
		for _, k := range desiredOrder {
			v := desired[k]
			if v == "" {
				continue
			}
			section = append(section, fmt.Sprintf("  %s: %s", k, yamlQuoteIfNeeded(v)))
		}
		out := append([]string{}, section...)
		out = append(out, lines...)
		return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	}

	// Find the extent of the model: section (indent > 0 until next top-level key).
	end := len(lines)
	for i := modelIdx + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if trimmed != "" && indent == 0 {
			end = i
			break
		}
	}

	// Parse existing sub-keys to preserve unknown ones (context_length, max_tokens, etc.).
	existing := map[string]string{}
	existingOrder := []string{}
	for i := modelIdx + 1; i < end; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// key: value
		colon := strings.Index(trimmed, ":")
		if colon < 0 {
			continue
		}
		k := strings.TrimSpace(trimmed[:colon])
		v := strings.TrimSpace(trimmed[colon+1:])
		existing[k] = v
		existingOrder = append(existingOrder, k)
	}

	// Merge: desired overrides existing; unknown existing keys are kept.
	merged := map[string]string{}
	for _, k := range existingOrder {
		merged[k] = existing[k]
	}
	for _, k := range desiredOrder {
		v := desired[k]
		if v != "" {
			merged[k] = v
		}
	}

	// Build the new section preserving original key order, with desired keys
	// inserted in order if not already present.
	seen := map[string]bool{}
	newSection := []string{"model:"}
	for _, k := range existingOrder {
		v := merged[k]
		newSection = append(newSection, fmt.Sprintf("  %s: %s", k, yamlQuoteIfNeeded(v)))
		seen[k] = true
	}
	for _, k := range desiredOrder {
		if seen[k] {
			continue
		}
		v := merged[k]
		if v == "" {
			continue
		}
		newSection = append(newSection, fmt.Sprintf("  %s: %s", k, yamlQuoteIfNeeded(v)))
	}

	// Reassemble.
	out := append([]string{}, lines[:modelIdx]...)
	out = append(out, newSection...)
	out = append(out, lines[end:]...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

// yamlQuoteIfNeeded wraps a value in double quotes if it contains characters
// that would be ambiguous in YAML (colons, hashes, etc.) or looks like a URL.
func yamlQuoteIfNeeded(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return `""`
	}
	// Already quoted?
	if (strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`)) ||
		(strings.HasPrefix(v, `'`) && strings.HasSuffix(v, `'`)) {
		return v
	}
	// Quote if it contains special chars or looks like a URL/path.
	if strings.ContainsAny(v, ":#@/?&=") || strings.Contains(v, " ") {
		return `"` + v + `"`
	}
	return v
}

func backupToolConfig(toolID string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	var configPaths []string
	switch toolID {
	case "claude":
		configPaths = []string{filepath.Join(homeDir, ".claude", "settings.json")}
	case "opencode":
		configPaths = []string{filepath.Join(homeDir, ".config", "opencode", "opencode.json")}
	case "cline":
		configPaths = []string{
			filepath.Join(homeDir, ".cline", "data", "globalState.json"),
			filepath.Join(homeDir, ".cline", "data", "secrets.json"),
		}
	case "codex":
		configPaths = []string{
			filepath.Join(homeDir, ".codex", "config.toml"),
			filepath.Join(homeDir, ".codex", "auth.json"),
		}
	case "kilo", "kilocode":
		configPaths = []string{filepath.Join(homeDir, ".local", "share", "kilo", "auth.json")}
	case "deepseek":
		configPaths = []string{filepath.Join(homeDir, ".deepseek", "config.toml")}
	case "jcode":
		configPaths = []string{
			filepath.Join(homeDir, ".jcode", "config.toml"),
			filepath.Join(homeDir, ".config", "jcode", "provider-9router.env"),
		}
	case "hermes":
		configPaths = []string{
			filepath.Join(homeDir, ".hermes", "config.yaml"),
			filepath.Join(homeDir, ".hermes", ".env"),
		}
	case "droid":
		configPaths = []string{filepath.Join(homeDir, ".factory", "settings.json")}
	case "openclaw":
		configPaths = []string{filepath.Join(homeDir, ".openclaw", "openclaw.json")}
	case "copilot":
		configPaths = []string{filepath.Join(homeDir, ".config", "Code", "User", "chatLanguageModels.json")}
	default:
		return ""
	}

	var firstBackup string
	for _, p := range configPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if isOmniProxyActiveProvider(data) {
			continue
		}
		backupPath := fmt.Sprintf("%s.omniproxy.bak.%d", p, time.Now().Unix())
		if err := os.WriteFile(backupPath, data, 0644); err != nil {
			continue
		}
		if firstBackup == "" {
			firstBackup = backupPath
		}
	}
	return firstBackup
}

type ToolStatus struct {
	Installed    bool `json:"installed"`
	HasOmniProxy bool `json:"hasOmniProxy"`
}

// getToolConfigPaths returns config file paths for a tool (shared across checks)
func getToolConfigPaths(homeDir, toolID string) []string {
	switch toolID {
	case "claude":
		return []string{filepath.Join(homeDir, ".claude", "settings.json")}
	case "opencode":
		return []string{filepath.Join(homeDir, ".config", "opencode", "opencode.json")}
	case "cline":
		return []string{
			filepath.Join(homeDir, ".cline", "data", "globalState.json"),
			filepath.Join(homeDir, ".cline", "data", "secrets.json"),
		}
	case "codex":
		return []string{
			filepath.Join(homeDir, ".codex", "config.toml"),
			filepath.Join(homeDir, ".codex", "auth.json"),
		}
	case "kilo", "kilocode":
		return []string{filepath.Join(homeDir, ".local", "share", "kilo", "auth.json")}
	case "deepseek":
		return []string{filepath.Join(homeDir, ".deepseek", "config.toml")}
	case "jcode":
		return []string{
			filepath.Join(homeDir, ".jcode", "config.toml"),
			filepath.Join(homeDir, ".config", "jcode", "provider-9router.env"),
		}
	case "hermes":
		return []string{
			filepath.Join(homeDir, ".hermes", "config.yaml"),
			filepath.Join(homeDir, ".hermes", ".env"),
		}
	case "droid":
		return []string{filepath.Join(homeDir, ".factory", "settings.json")}
	case "openclaw":
		return []string{filepath.Join(homeDir, ".openclaw", "openclaw.json")}
	case "copilot":
		return []string{filepath.Join(homeDir, ".config", "Code", "User", "chatLanguageModels.json")}
	default:
		return nil
	}
}

func checkToolInstalled(toolID string) bool {
	switch toolID {
	case "copilot":
		home, _ := os.UserHomeDir()
		path := filepath.Join(home, ".config", "Code", "User", "chatLanguageModels.json")
		_, err := os.Stat(path)
		return err == nil
	case "cursor":
		home, _ := os.UserHomeDir()
		_, err := os.Stat(filepath.Join(home, ".cursor"))
		return err == nil
	case "mitm", "antigravity", "kiro":
		return true
	default:
		cmd := exec.Command("which", toolID)
		return cmd.Run() == nil
	}
}

func checkToolHasOmniProxy(toolID string) bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	var configExists bool

	switch toolID {
	case "claude":
		path := filepath.Join(homeDir, ".claude", "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return false
		}
		env, _ := cfg["env"].(map[string]interface{})
		if env != nil {
			if url, _ := env["ANTHROPIC_BASE_URL"].(string); url != "" {
				return true
			}
		}
		return false

	case "opencode":
		path := filepath.Join(homeDir, ".config", "opencode", "opencode.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return false
		}
		model, _ := cfg["model"].(string)
		if strings.HasPrefix(model, "omniproxy/") {
			return true
		}
		return false

	case "codex":
		path := filepath.Join(homeDir, ".codex", "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		// Only the active model_provider matters — the section may exist
		// but if another provider is selected the tool isn't connected to us
		return strings.Contains(string(data), "model_provider = \"omniproxy\"") ||
			strings.Contains(string(data), "model_provider = \"superkiro\"")

	case "cline":
		path := filepath.Join(homeDir, ".cline", "data", "globalState.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return false
		}
		if url, _ := cfg["openAiBaseUrl"].(string); url != "" {
			return true
		}
		return false

	case "kilo":
		path := filepath.Join(homeDir, ".local", "share", "kilo", "auth.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return false
		}
		if entry, _ := cfg["openai-compatible"].(map[string]interface{}); entry != nil {
			if url, _ := entry["baseUrl"].(string); url != "" {
				return true
			}
		}
		return false

	case "deepseek":
		path := filepath.Join(homeDir, ".deepseek", "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		content := strings.ToLower(string(data))
		return strings.Contains(content, "provider = \"openai\"")

	case "jcode":
		path := filepath.Join(homeDir, ".jcode", "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		return strings.Contains(string(data), "[providers.9router]") ||
			strings.Contains(strings.ToLower(string(data)), "omniproxy") ||
			strings.Contains(strings.ToLower(string(data)), "superkiro")

	case "hermes":
		path := filepath.Join(homeDir, ".hermes", "config.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		return strings.Contains(string(data), "omniproxy:") ||
			strings.Contains(string(data), "superkiro:")

	case "droid":
		path := filepath.Join(homeDir, ".factory", "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return false
		}
		if models, _ := cfg["customModels"].([]interface{}); models != nil {
			for _, m := range models {
				if mm, _ := m.(map[string]interface{}); mm != nil {
					if id, _ := mm["id"].(string); strings.HasPrefix(id, "custom:9Router") {
						return true
					}
				}
			}
		}
		return false

	case "openclaw":
		path := filepath.Join(homeDir, ".openclaw", "openclaw.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		return strings.Contains(string(data), "\"omniproxy\"") ||
			strings.Contains(string(data), "\"superkiro\"")

	case "copilot":
		path := filepath.Join(homeDir, ".config", "Code", "User", "chatLanguageModels.json")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		var entries []map[string]interface{}
		if json.Unmarshal(data, &entries) != nil {
			return false
		}
		for _, e := range entries {
			if title, _ := e["title"].(string); strings.EqualFold(title, "OmniProxy") || strings.EqualFold(title, "SuperKiro") {
				return true
			}
		}
		return false

	case "cursor", "continue", "roo", "amp", "qwen", "cowork":
		break

	case "mitm", "antigravity", "kiro":
		break
	}

	if !configExists {
		cliToolConfiguredMu.RLock()
		_, applied := cliToolConfigured[toolID]
		cliToolConfiguredMu.RUnlock()
		return applied
	}
	return false
}

func getCliToolsStatus() map[string]ToolStatus {
	ids := []string{
		"claude", "opencode", "cline", "codex", "kilo",
		"continue", "roo", "deepseek", "jcode", "hermes",
		"droid", "openclaw", "cursor", "amp", "qwen",
		"cowork", "mitm", "antigravity", "copilot", "kiro",
	}
	out := make(map[string]ToolStatus, len(ids))
	for _, id := range ids {
		out[id] = ToolStatus{
			Installed:    checkToolInstalled(id),
			HasOmniProxy: checkToolHasOmniProxy(id),
		}
	}
	return out
}

type cliBackupWriter struct {
	http.ResponseWriter
	backupFile  string
	wroteHeader bool
}

func (w *cliBackupWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader && statusCode == http.StatusOK && w.backupFile != "" {
		w.Header().Set("X-Cli-Backup", w.backupFile)
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

// Handler is the HTTP handler
type Handler struct {
	pool *pool.AccountPool
	// runtime stats (using atomic operations)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 needs mutex protection
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// model cache
	webDir          string
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	promptCache     *promptCacheTracker
	tokenRefreshMu  sync.Mutex
	usageTracker    *UsageTracker
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// apply proxy config at startup
	applyProxyConfig(config.GetProxyURL())
	// load compression settings from config (KVSettings)
	InitCompressionConfig()

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:       time.Now().Unix(),
		stopRefresh:     make(chan struct{}),
		stopStatsSaver:  make(chan struct{}),
		promptCache:     newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker:    GetUsageTracker(),
	}
	// Resolve web assets dir relative to the binary so the server works
	// regardless of the current working directory.
	if exe, err := os.Executable(); err == nil {
		h.webDir = filepath.Join(filepath.Dir(exe), "web")
	} else {
		h.webDir = "web"
	}
	// start background refresh
	go h.backgroundRefresh()
	// start background stats saver (every 30s)
	go h.backgroundStatsSaver()
	// clean up expired stored responses (>30 days)
	go purgeExpiredResponses(responsesDefaultTTL)
	return h
}

// syncNineRouterAccounts imports every credentialed connection from the local
// 9router database. It is best-effort: a missing 9router installation must not
// prevent OmniProxy from starting. The existing import methods provide the
// stable SourceID deduplication and capability metadata used by the UI/router.
func (h *Handler) syncNineRouterAccounts() {
	result, err := auth.ReadNineRouterDB()
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("[9router] startup sync skipped: %v", err)
		}
		return
	}

	imported, failed := 0, 0
	for _, account := range result.Generic {
		if _, err := h.importOne9RouterGeneric(account); err != nil {
			failed++
			logger.Warnf("[9router] provider %s sync failed: %v", account.Provider, err)
		} else {
			imported++
		}
	}
	for _, account := range result.Codex {
		if _, err := h.importOne9RouterCodex(account); err != nil {
			failed++
			logger.Warnf("[9router] Codex sync failed: %v", err)
		} else {
			imported++
		}
	}
	for _, account := range result.Kiro {
		// Do not refresh OAuth tokens during startup. The account is still
		// synchronized and can be validated from the account detail UI.
		if _, err := h.importOne9RouterKiro(account, false); err != nil {
			failed++
			logger.Warnf("[9router] Kiro sync failed: %v", err)
		} else {
			imported++
		}
	}

	if imported > 0 || failed > 0 {
		h.pool.Reload()
	}
	logger.Infof("[9router] startup sync complete: %d imported/updated, %d failed, search/image providers=%d", imported, failed, countServiceProviders(result.Generic))
}

func countServiceProviders(accounts []auth.NineRouterImportedAccount) int {
	count := 0
	for _, account := range accounts {
		if len(account.Capabilities) > 0 {
			count++
		}
	}
	return count
}

// backgroundRefresh periodically refreshes account info
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(10 * time.Minute) // refresh every 10 minutes (was 30)
	defer ticker.Stop()

	// run once after a 10s delay at startup
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
			h.pool.PruneCacheSticky()
			h.pool.PruneCacheWarmed()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts refreshes all account info
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		// Skip accounts with no access token (not yet authenticated).
		// Retry banned/disabled accounts — they may recover after re-registration.
		if account.AccessToken == "" {
			continue
		}
		// External OpenAI-compatible providers have no Kiro token to refresh
		// and no CodeWhisperer usage API to poll — skip them entirely.
		if isExternalAccount(account) || isServiceAccount(account) {
			continue
		}

		// Check if the token needs refresh. Codex accounts can carry a stale
		// ExpiresAt from import, so also inspect the JWT exp claim.
		needsRefresh := account.ExpiresAt > 0 && time.Now().Unix() >= account.ExpiresAt-tokenRefreshSkewSeconds
		if account.AuthMethod == codexAuthMethod {
			needsRefresh = codexTokenNeedsRefresh(account, time.Now().Unix())
		}
		if isAntigravityAccount(account) {
			needsRefresh = antigravityTokenNeedsRefresh(account, time.Now().Unix())
		}
		if needsRefresh {
			if isAntigravityAccount(account) {
				if err := refreshAntigravityAccountToken(account); err != nil {
					logger.Warnf("[BackgroundRefresh] Antigravity token refresh failed for %s: %v", account.Email, err)
					continue
				}
				h.pool.UpdateToken(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt)
			} else if account.AuthMethod == codexAuthMethod {
				if err := refreshCodexAccountToken(account); err != nil {
					markCodexAuthFailure(account, err)
					logger.Warnf("[BackgroundRefresh] Codex token refresh failed for %s: %v", account.Email, err)
					// Never probe usage with an access token that is known to be
					// expired when the refresh-token flow failed.
					continue
				}
				h.pool.UpdateToken(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt)
			} else {
				newAccessToken, newRefreshToken, newExpiresAt, profileArn, newClientID, newClientSecret, err := auth.RefreshAccountToken(account)
				if err != nil {
					logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
					h.handleAccountFailure(account, err, "")
					continue
				}
				account.AccessToken = newAccessToken
				if newRefreshToken != "" {
					account.RefreshToken = newRefreshToken
				}
				account.ExpiresAt = newExpiresAt
				config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
				h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
				if profileArn != "" {
					account.ProfileArn = profileArn
					config.UpdateAccountProfileArn(account.ID, profileArn)
				}
				// Codex accounts: re-extract chatgpt_account_id from the new
				// access-token JWT (it may rotate when OpenAI re-issues the
				// user's account) and persist it.
				if account.AuthMethod == codexAuthMethod {
					account.AccessToken = newAccessToken
					refreshCodexAccountID(account)
				}
				// Persist re-registered OIDC client credentials so next refresh cycle uses them.
				if newClientID != "" && newClientSecret != "" {
					account.ClientID = newClientID
					account.ClientSecret = newClientSecret
					config.UpdateAccount(account.ID, *account)
				}
				// Re-enable banned/disabled account if refresh succeeded.
				if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
					account.BanStatus = "ACTIVE"
					account.BanReason = ""
					account.BanTime = 0
					account.Enabled = true
					config.UpdateAccount(account.ID, *account)
					logger.Infof("[BackgroundRefresh] Re-enabled %s after successful refresh", account.Email)
				}
			}
		}

		// Codex accounts: backfill JWT profile (email, name, plan_type)
		// if missing. This runs on every refresh cycle but only persists
		// when the fields are empty or changed — covers accounts imported
		// before the profile extraction was added. Must run BEFORE the
		// Kiro RefreshAccountInfo block, which would `continue` on error
		// for Codex accounts (they can't call CodeWhisperer APIs).
		if account.AuthMethod == codexAuthMethod && account.AccessToken != "" {
			refreshCodexAccountID(account)
		}

		// refresh account info (skip for external IdP, Codex and Antigravity —
		// their tokens cannot call CodeWhisperer usage APIs).
		if account.AuthMethod != "external_idp" && account.AuthMethod != codexAuthMethod && !isAntigravityAccount(account) && (account.BanStatus == "" || account.BanStatus == "ACTIVE") {
			info, err := RefreshAccountInfo(account)
			if err != nil {
				errMsg := err.Error()
				if isSuspensionErrorMessage(strings.ToLower(errMsg)) {
					account.BanStatus = "BANNED"
					account.BanReason = truncateErrBody([]byte(errMsg))
					account.BanTime = time.Now().Unix()
					account.Enabled = false
					config.UpdateAccount(account.ID, *account)
					logger.Warnf("[BackgroundRefresh] Marked %s as BANNED: %s", account.Email, errMsg)
				} else {
					logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
				}
				continue
			}
			config.UpdateAccountInfo(account.ID, *info)
			logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
		}

		// Codex accounts: fetch live usage (rate-limit %, credits) via a minimal
		// request so the admin UI shows real-time token usage. Authentication
		// failures are token/account-health signals, not proof of a ban.
		if account.AuthMethod == codexAuthMethod && account.AccessToken != "" {
			if err := fetchCodexUsage(account); err != nil {
				markCodexAuthFailure(account, err)
				logger.Warnf("[BackgroundRefresh] Codex usage fetch failed for %s: %v", account.Email, err)
			}
		}
	}
	h.pool.Reload()
}

// validateApiKey validates API Key (Bool wrapper, old sig still used by some callers)
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP routes requests
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - full header support
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// routing
	switch {
	// API endpoints (require API Key auth)
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/search" || path == "/search":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleSearch(w, ar)
	case path == "/v1/images/generations" || path == "/images/generations":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleImageGeneration(w, ar)
	// Video generation is asynchronous upstream. The create route waits for the
	// finished asset so a simple client needs one call, and the status route
	// exists for jobs that outlive that wait rather than being lost.
	case path == "/v1/videos/generations" || path == "/videos/generations":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.apiGommoCreateVideo(w, ar)
	case strings.HasPrefix(path, "/v1/videos/") || strings.HasPrefix(path, "/videos/"):
		jobID := strings.TrimPrefix(strings.TrimPrefix(path, "/v1/videos/"), "/videos/")
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.apiGommoVideoStatus(w, ar, jobID)
	// Capability passthrough endpoints (embeddings, audio, image edits,
	// moderations). Routing is driven by the account capability table rather
	// than a per-endpoint provider switch, so a provider that starts serving a
	// new capability becomes reachable after a catalog refresh with no code
	// change.
	case capabilityRouteMatches(path):
		route, _ := lookupCapabilityEndpoint(path)
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleCapabilityPassthrough(w, ar, route)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case strings.HasPrefix(path, "/v1/models/") || strings.HasPrefix(path, "/models/"):
		modelID := strings.TrimPrefix(path, "/v1/models/")
		modelID = strings.TrimPrefix(modelID, "/models/")
		h.handleModelByID(w, r, modelID)
	case path == "/api/event_logging/batch":
		// Claude Code telemetry endpoint - return 200 OK directly
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// admin endpoints
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// health check
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// stats endpoint (requires API Key auth)
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth health check (does not expose statistics)
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats returns stats (requires API Key auth)
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	var kiroUsageCurrent, kiroUsageLimit, trialUsageCurrent, trialUsageLimit float64
	var codexAccounts, codexEnabled int
	var codexPrimaryPctSum, codexSecondaryPctSum int
	var codexCreditsBalance int
	for _, a := range h.pool.GetAllAccounts() {
		if isCodexAccount(&a) {
			codexAccounts++
			if a.Enabled {
				codexEnabled++
				codexPrimaryPctSum += a.CodexPrimaryUsedPercent
				codexSecondaryPctSum += a.CodexSecondaryUsedPercent
				codexCreditsBalance += a.CodexCreditsBalance
			}
		} else {
			kiroUsageCurrent += a.UsageCurrent
			kiroUsageLimit += a.UsageLimit
			trialUsageCurrent += a.TrialUsageCurrent
			trialUsageLimit += a.TrialUsageLimit
		}
	}
	h.modelsCacheMu.RLock()
	cachedModels := h.cachedModels
	h.modelsCacheMu.RUnlock()
	modelIds := make([]string, 0, len(cachedModels))
	for _, m := range cachedModels {
		if m.ModelId != "" && m.ModelId != "auto" {
			modelIds = append(modelIds, m.ModelId)
		}
	}
	// Include Codex models in the count when enabled.
	codexModelCount := 0
	if codexEnabled > 0 {
		codexModelCount = len(codexSubscriptionModelsList())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":            "ok",
		"version":           config.Version,
		"accounts":          h.pool.Count(),
		"available":         h.pool.AvailableCount(),
		"totalRequests":     atomic.LoadInt64(&h.totalRequests),
		"successRequests":   atomic.LoadInt64(&h.successRequests),
		"failedRequests":    atomic.LoadInt64(&h.failedRequests),
		"totalTokens":       atomic.LoadInt64(&h.totalTokens),
		"totalCredits":      h.getCredits(),
		"kiroUsageCurrent":  kiroUsageCurrent,
		"kiroUsageLimit":    kiroUsageLimit,
		"trialUsageCurrent": trialUsageCurrent,
		"trialUsageLimit":   trialUsageLimit,
		"codexAccounts":     codexAccounts,
		"codexEnabled":      codexEnabled,
		"codexPrimaryUsedAvg": func() int {
			if codexEnabled == 0 {
				return 0
			}
			return codexPrimaryPctSum / codexEnabled
		}(),
		"codexSecondaryUsedAvg": func() int {
			if codexEnabled == 0 {
				return 0
			}
			return codexSecondaryPctSum / codexEnabled
		}(),
		"codexCreditsBalance": codexCreditsBalance,
		"availableModels":     len(modelIds) + codexModelCount,
		"modelIds":            modelIds,
		"uptime":              time.Now().Unix() - h.startTime,
	})
}

// canonicalClaude5ModelIDs is the public Claude catalog. Thinking is a request
// capability parsed by ParseModelAndThinking; it is deliberately not exposed as
// a second model ID in discovery responses.
var canonicalClaude5ModelIDs = []string{
	"claude-sonnet-5",
	"claude-opus-5",
}

func canonicalClaude5Models() []map[string]interface{} {
	models := make([]map[string]interface{}, 0, len(canonicalClaude5ModelIDs))
	for _, id := range canonicalClaude5ModelIDs {
		entry := buildModelInfo(id, "anthropic", true)
		entry["token_limits"] = map[string]interface{}{
			"maxInputTokens":  1_000_000,
			"maxOutputTokens": 128_000,
		}
		models = append(models, entry)
	}
	return models
}

// mergePublicModelEntries deduplicates catalog entries by lowercase model ID
// while preserving first-seen order, so canonical metadata wins over a later
// discovery-cache record for the same ID.
func mergePublicModelEntries(models []map[string]interface{}) []map[string]interface{} {
	seen := make(map[string]bool, len(models))
	out := make([]map[string]interface{}, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, model)
	}
	return out
}

// handleModels returns the configured public catalog. The legacy response
// contains canonical Claude and Codex models. Rich consumers may request the
// account-aware catalogue with ?catalog=all.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	models := canonicalClaude5Models()
	if hasEnabledCodexAccount() {
		models = append(models, codexSubscriptionModelsList()...)
	}
	if r.URL.Query().Get("catalog") == "all" {
		h.modelsCacheMu.RLock()
		cached := append([]ModelInfo(nil), h.cachedModels...)
		h.modelsCacheMu.RUnlock()
		for _, info := range cached {
			id := strings.TrimSpace(info.ModelId)
			if id == "" {
				continue
			}
			ownedBy := strings.TrimSpace(info.Provider)
			if ownedBy == "" {
				ownedBy = "external"
			}
			models = append(models, buildModelInfoWithTokenLimits(id, ownedBy, modelSupportsImage(info.InputTypes), &info))
		}
		models = mergePublicModelEntries(models)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}


// handleModelByID returns a single entry from the public catalog.
func (h *Handler) handleModelByID(w http.ResponseWriter, r *http.Request, modelID string) {
	if modelID == "" {
		h.sendClaudeError(w, 404, "not_found_error", "Model ID is required")
		return
	}

	models := canonicalClaude5Models()
	if hasEnabledCodexAccount() {
		models = append(models, codexSubscriptionModelsList()...)
	}
	for _, model := range models {
		if model["id"] == modelID {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(model)
			return
		}
	}

	h.sendClaudeError(w, 404, "not_found_error", "Model not found: "+modelID)
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			ownedBy := m.Provider
			if ownedBy == "" {
				// Older cache entries predate provider metadata and are native
				// Kiro models, so preserve the previous Anthropic-compatible
				// discovery behavior for those entries.
				ownedBy = "anthropic"
			}
			models = append(models, buildModelInfoWithTokenLimits(m.ModelId, ownedBy, supportsImage, &m))
			// auto-generate thinking variants
			models = append(models, buildModelInfoWithTokenLimits(m.ModelId+thinkingSuffix, ownedBy, supportsImage, &m))
		}
	}
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	return buildModelInfoWithTokenLimits(id, ownedBy, supportsImage, nil)
}

// buildModelInfoWithTokenLimits keeps model discovery consistent with runtime
// accounting. Claude Code uses the advertised limits when deciding whether
// to compact or continue a conversation, so omitting them can make a valid
// 1M-context model look like an unknown or smaller-window model.
func buildModelInfoWithTokenLimits(id, ownedBy string, supportsImage bool, source *ModelInfo) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	entry := map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]interface{}{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
			"image_input":  map[string]bool{"supported": supportsImage},
			"streaming":    map[string]bool{"supported": true},
			"tool_use":     map[string]bool{"supported": true},
			"reasoning":    map[string]interface{}{"supported": true, "type": "adaptive"},
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}

	var info ModelInfo
	if source != nil {
		info = *source
		info.ModelId = id
	} else {
		info.ModelId = id
	}
	input, output, ok := modelInfoTokenLimits(info)
	if ok && (input > 0 || output > 0) {
		entry["token_limits"] = map[string]interface{}{
			"maxInputTokens":  input,
			"maxOutputTokens": output,
		}
	}
	return entry
}

// hasEnabledCodexAccount returns true if at least one enabled Codex
// account exists in the config. Used to decide whether to advertise
// Codex subscription models in /v1/models.
func hasEnabledCodexAccount() bool {
	for _, a := range config.GetEnabledAccounts() {
		if isCodexAccount(&a) {
			return true
		}
	}
	return false
}

// codexSubscriptionModelsList returns the Codex subscription model list
// in the /v1/models response format (map[string]interface{}). Derives
// from the same codexSubscriptionModels() source as the routing cache so
// both surfaces stay in sync.
func codexSubscriptionModelsList() []map[string]interface{} {
	models := codexSubscriptionModels()
	out := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		entry := buildModelInfo(m.ModelId, "openai-codex", true)
		entry["name"] = m.ModelName
		entry["description"] = m.Description
		if m.TokenLimits != nil {
			entry["token_limits"] = map[string]interface{}{
				"maxInputTokens":  m.TokenLimits.MaxInputTokens,
				"maxOutputTokens": m.TokenLimits.MaxOutputTokens,
			}
		}
		out = append(out, entry)
	}
	return out
}

// mergeCanonicalModelResponseEntries replaces discovery entries whose IDs are
// also published by a canonical catalog. This prevents an upstream cache entry
// from shadowing Codex token metadata in /v1/models and /v1/models/{id}.
func mergeCanonicalModelResponseEntries(models, canonical []map[string]interface{}) []map[string]interface{} {
	indexByID := make(map[string]int, len(models)+len(canonical))
	merged := make([]map[string]interface{}, 0, len(models)+len(canonical))
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			merged = append(merged, model)
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = model
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}
	for _, model := range canonical {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			merged = append(merged, model)
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = model
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}
	return merged
}

// refreshModelsCache fetches model list from Kiro API and caches it
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	aggregated := make([]ModelInfo, 0)
	for i := range accounts {
		account := &accounts[i]
		// Skip external IdP (enterprise SSO) accounts — Azure AD tokens cannot call
		// the CodeWhisperer ListAvailableModels endpoint.
		if account.AuthMethod == "external_idp" {
			continue
		}
		// Codex accounts: seed the fixed subscription model list into the cache
		// (no /v1/models endpoint to poll). Also set per-account model list so
		// routing knows which models this account can serve.
		if isCodexAccount(account) {
			codexModels := codexSubscriptionModels()
			modelIDs := make([]string, 0, len(codexModels))
			for _, m := range codexModels {
				modelIDs = append(modelIDs, m.ModelId)
			}
			h.pool.SetModelList(account.ID, modelIDs)
			aggregated = mergeUniqueModels(aggregated, codexModels)
			continue
		}
		// Antigravity accounts answer their own catalog action and never
		// CodeWhisperer's, so they are served the same way as the external pool.
		if isAntigravityAccount(account) {
			if err := h.fetchAndCacheAccountModels(account); err == nil {
				h.modelsCacheMu.RLock()
				aggregated = mergeUniqueModels(aggregated, h.cachedModels)
				h.modelsCacheMu.RUnlock()
			}
			continue
		}
		// Skip external OpenAI-compatible providers — they have no Kiro token and
		// their model list comes from {BaseURL}/v1/models via fetchExternalProviderModels,
		// not CodeWhisperer's ListAvailableModels. Calling ListAvailableModels with
		// their access token fails (DNS/auth) and triggers handleAccountFailure,
		// which can wrongly mark the account BANNED.
		// Antigravity accounts are in the same position: their catalog comes from
		// Cloud Code Assist's own fetchAvailableModels, and a Google OAuth token
		// cannot call ListAvailableModels at all.
		if isExternalAccount(account) || isServiceAccount(account) || isAntigravityAccount(account) {
			if err := h.fetchAndCacheAccountModels(account); err == nil {
				h.modelsCacheMu.RLock()
				aggregated = mergeUniqueModels(aggregated, h.cachedModels)
				h.modelsCacheMu.RUnlock()
			}
			continue
		}
		if err := h.ensureValidToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			h.handleAccountFailure(account, err, "")
			continue
		}

		models, err := ListAvailableModels(account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err, "")
			continue
		}
		for i := range models {
			if models[i].Provider == "" {
				models[i].Provider = "kiro-proxy"
			}
		}
		// Cache available models per account, used for filtering during routing
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

// fetchAndCacheAccountModels fetches and writes model cache for a single account.
// Also updates the pool routing cache and global aggregated model list.
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	// Gommo is checked before the service-account guard below: a Gommo account
	// carries the image capability, so that guard would otherwise reject it as
	// "no chat models" and leave its media catalog permanently empty.
	if isGommoAccount(account) {
		models, err := fetchGommoModels(account)
		if err != nil {
			return err
		}
		if len(models) == 0 {
			return fmt.Errorf("gommo account %s returned an empty model catalog", account.Email)
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		h.modelsCacheMu.Lock()
		h.cachedModels = mergeUniqueModels(h.cachedModels, models)
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d Gommo media models for %s", len(models), account.Email)
		return nil
	}
	if isServiceAccount(account) {
		return fmt.Errorf("service account %s does not expose chat models", account.Provider)
	}
	// Codex accounts expose a fixed set of subscription-tier models
	// (gpt-5.6-sol, gpt-5.1, o4, etc.) — no /v1/models endpoint to poll.
	// Seed the cache with the canonical Codex model list so routing picks
	// them up.
	if isCodexAccount(account) {
		models := codexSubscriptionModels()
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		h.modelsCacheMu.Lock()
		h.cachedModels = mergeUniqueModels(h.cachedModels, models)
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Seeded %d Codex subscription models for %s", len(models), account.Email)
		return nil
	}
	// Antigravity publishes its catalog through fetchAvailableModels. The call
	// can fail while the credential is still good (the action is not available
	// on every environment), so a verified static list backs it rather than
	// leaving the account with no routable models.
	if isAntigravityAccount(account) {
		models, err := fetchAntigravityModels(account)
		if err != nil || len(models) == 0 {
			models = antigravityFallbackModels()
			if err != nil {
				logger.Warnf("[ModelsCache] Antigravity catalog fetch failed for %s (%v); using verified fallback list", account.Email, err)
			}
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		h.modelsCacheMu.Lock()
		h.cachedModels = mergeUniqueModels(h.cachedModels, models)
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Seeded %d Antigravity models for %s", len(models), account.Email)
		return nil
	}
	// External OpenAI-compatible providers expose /v1/models, not Kiro's
	if isAgentRouterAccount(account) {
		models, err := fetchAgentRouterModels(account)
		if err == nil && len(models) > 0 {
			modelIDs := make([]string, 0, len(models))
			for _, m := range models {
				modelIDs = append(modelIDs, m.ModelId)
			}
			h.pool.SetModelList(account.ID, modelIDs)
			h.modelsCacheMu.Lock()
			h.cachedModels = mergeUniqueModels(h.cachedModels, models)
			h.modelsCacheTime = time.Now().Unix()
			h.modelsCacheMu.Unlock()
			logger.Infof("[ModelsCache] Refreshed %d models for AgentRouter provider %s", len(models), account.Email)
			return nil
		}
	}
	if isExternalAccount(account) {
		models, err := fetchExternalProviderModels(account)
		if err != nil {
			return err
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		// Classify what this provider can actually serve from its own catalog
		// instead of inferring capabilities from the provider name. Persisted
		// only when the set changed, so a periodic refresh does not rewrite
		// config on every cycle.
		if applyDiscoveredCapabilities(account, models) {
			if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
				logger.Warnf("[Capabilities] Failed to persist discovered capabilities for %s: %v", account.Email, err)
			} else {
				logger.Infof("[Capabilities] %s -> %s", account.Email, strings.Join(account.DiscoveredCapabilities, ", "))
			}
		}
		h.pool.SetModelList(account.ID, modelIDs)
		h.modelsCacheMu.Lock()
		h.cachedModels = mergeUniqueModels(h.cachedModels, models)
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Refreshed %d models for external provider %s", len(models), account.Email)
		return nil
	}
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	for i := range models {
		if models[i].Provider == "" {
			models[i].Provider = "kiro-proxy"
		}
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// merge into aggregate cache
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	return nil
}

// refreshExternalCredits fetches the external provider's /api/me endpoint and
// persists the credit/usage snapshot onto the account so the admin UI can
// render remaining credits. Non-fatal: returns error when the provider does
// not implement /api/me (caller decides whether to surface it).
func (h *Handler) refreshExternalCredits(account *config.Account) error {
	me, err := fetchExternalProviderCredits(account)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if err := config.UpdateAccountExternalCredits(
		account.ID,
		me.CreditLimit, me.CreditsRemaining, me.CreditsUsed,
		me.RequestsCount, me.TokensUsed,
		me.Status, me.KeyMasked, me.LastUsedAt, now,
	); err != nil {
		return fmt.Errorf("persist credits: %w", err)
	}
	// Mirror onto the in-memory account so the next /admin/api/accounts read
	// reflects the new values without a full reload.
	account.ExtCreditLimit = me.CreditLimit
	account.ExtCreditsRemaining = me.CreditsRemaining
	account.ExtCreditsUsed = me.CreditsUsed
	account.ExtRequestsCount = me.RequestsCount
	account.ExtTokensUsed = me.TokensUsed
	account.ExtStatus = me.Status
	account.ExtKeyMasked = me.KeyMasked
	account.ExtLastUsedAt = me.LastUsedAt
	account.ExtCreditsCheckedAt = now
	logger.Infof("[ExternalCredits] %s: remaining=%.2f used=%.2f limit=%.2f requests=%d status=%s",
		account.Email, me.CreditsRemaining, me.CreditsUsed, me.CreditLimit, me.RequestsCount, me.Status)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// Immediately fetches and updates the model routing cache for a specific account.
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// get latest token from pool at runtime (same logic as refreshModelsCache)
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// Reuses refreshModelsCache to refresh model routing cache for all enabled accounts.
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCache()
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

// apiRefreshAllAccounts POST /admin/api/accounts/refresh-all
// Runs the SAME per-account refresh as the refresh button on each account card
// (refreshAccountFull), for every account, and returns a summary.
//
// It previously delegated to refreshAllAccounts(), the background scheduler's
// conservative pass: that skips external and service accounts, refreshes the
// token only when near expiry, never refreshes model catalogs or external
// credits, and never clears a stale ban. "Refresh All" therefore did strictly
// less work than clicking refresh on each card, which is not what the label
// promises.
func (h *Handler) apiRefreshAllAccounts(w http.ResponseWriter, r *http.Request) {
	outcomes := h.refreshAllAccountsFull()

	refreshed, banned, reauthRequired, failed, skipped := 0, 0, 0, 0, 0
	for _, o := range outcomes {
		switch {
		case o.Skipped:
			skipped++
		case o.Err != nil:
			failed++
		case o.Reauth:
			reauthRequired++
		case o.Banned:
			banned++
		default:
			refreshed++
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"refreshed":      refreshed,
		"banned":         banned,
		"reauthRequired": reauthRequired,
		"failed":         failed,
		"skipped":        skipped,
		"message":        fmt.Sprintf("Refreshed %d, banned %d, re-login required %d, failed %d, skipped %d", refreshed, banned, reauthRequired, failed, skipped),
	})
}

// apiResetAccountQuota POST /admin/api/accounts/{id}/reset-quota
// Clears the Codex primary/secondary usage counters and reset timestamps so
// the pool treats the account as fully available again. Useful when an
// operator knows the upstream quota has been reset (e.g. after a billing
// cycle change) and wants to skip waiting for the natural cooldown.
//
// Only meaningful for Codex accounts — Kiro accounts have their quota
// refreshed via /refresh, and External OpenAI accounts via /credits.
func (h *Handler) apiResetAccountQuota(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	if !isCodexAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "reset-quota is only supported for Codex accounts"})
		return
	}

	account.CodexPrimaryUsedPercent = 0
	account.CodexPrimaryResetAt = 0
	account.CodexTokensSincePrimaryReset = 0
	account.CodexPrimaryWindowTokensInitialized = true
	account.CodexPrimaryWindowMinutes = 0
	account.CodexSecondaryUsedPercent = 0
	account.CodexSecondaryResetAt = 0
	account.CodexUsageCheckedAt = 0
	// Clear any in-memory cooldown so the pool picks this account immediately.
	h.pool.ClearCooldown(account.ID)
	// Re-enable if it was disabled by the quota-exhaustion path.
	if !account.Enabled && account.BanStatus == "ACTIVE" {
		account.Enabled = true
	}
	if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to persist quota reset: " + err.Error()})
		return
	}
	h.pool.ResetCodexPrimaryWindowTokens(account.ID, 0)
	// Reload the pool so the weighted slice reflects the now-available account.
	h.pool.Reload()
	logger.Infof("[apiResetAccountQuota] Reset Codex quota for %s — account is now fully available", account.Email)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Quota reset for %s — account available immediately", account.Email),
	})
}

// apiGetResetCreditsAvailable GET /admin/api/accounts/{id}/reset-credits/available
// Queries the upstream Codex wham/usage endpoint and returns the number of
// bank-reset credits the account currently has available. The UI uses this
// to show a "Reset Credit available" button only when available_count > 0.
func (h *Handler) apiGetResetCreditsAvailable(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !isCodexAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "reset-credits is only supported for Codex accounts"})
		return
	}
	// Ensure token is fresh before querying upstream.
	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	available, err := codexResetCreditsAvailable(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"available": available,
	})
}

// apiResetAccountCredits POST /admin/api/accounts/{id}/reset-credits
// Consumes one Codex bank-reset credit upstream. On success, clears local
// Codex usage counters so the pool picks the account immediately. This is
// the "Bank Reset Quota" button — it only works if the account actually
// has a credit available upstream (rate_limit_reset_credits.available_count > 0).
func (h *Handler) apiResetAccountCredits(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !isCodexAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "reset-credits is only supported for Codex accounts"})
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	// Guard: a bank reset clears quota counters but does NOT clear a ban —
	// only a successful test request does. Consuming a credit on a banned
	// account would burn a non-refundable resource while the account stays
	// unroutable. Require the operator to clear the ban first (press Test).
	if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   false,
			"error":     "Account is banned (" + account.BanStatus + "). A bank reset does not clear a ban — press Test to clear it first, otherwise the credit would be spent on an unroutable account.",
			"banStatus": account.BanStatus,
		})
		return
	}
	// First check if the account actually has a credit available. This
	// avoids burning a consume request (and getting a no_credit response)
	// when the operator clicks the button on an account with no credits.
	available, err := codexResetCreditsAvailable(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to check available credits: " + err.Error()})
		return
	}
	if available <= 0 {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   false,
			"error":     "No bank-reset credits available for this account",
			"available": 0,
		})
		return
	}
	// Consume one credit upstream.
	windowsReset, err := codexConsumeResetCredit(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to consume reset credit: " + err.Error()})
		return
	}
	if windowsReset == 0 {
		// Upstream reported no_credit (race: another client consumed it
		// between our check and consume).
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   false,
			"error":     "No bank-reset credits available (consumed by another client)",
			"available": 0,
		})
		return
	}
	// Success — clear local Codex usage counters so the pool picks the
	// account immediately, and re-enable if it was disabled by the
	// quota-exhaustion path.
	account.CodexPrimaryUsedPercent = 0
	account.CodexPrimaryResetAt = 0
	account.CodexTokensSincePrimaryReset = 0
	account.CodexPrimaryWindowTokensInitialized = true
	account.CodexPrimaryWindowMinutes = 0
	account.CodexSecondaryUsedPercent = 0
	account.CodexSecondaryResetAt = 0
	account.CodexUsageCheckedAt = 0
	// Decrement cached bank-reset credit count (we just consumed one).
	if account.CodexResetCreditsAvailable > 0 {
		account.CodexResetCreditsAvailable--
	}
	h.pool.ClearCooldown(account.ID)
	// NOTE: we deliberately do NOT re-enable a disabled account here. The
	// enabled flag is an explicit operator decision (an account may be off
	// on purpose); silently flipping it would put the account back into the
	// routing pool without consent. We report the state instead so the UI
	// can tell the operator the reset landed but the account is still off.
	stillDisabled := !account.Enabled
	if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
		logger.Errorf("[apiResetAccountCredits] Failed to persist cleared counters for %s: %v", account.Email, err)
	}
	h.pool.ResetCodexPrimaryWindowTokens(account.ID, 0)
	h.pool.Reload()
	logger.Infof("[apiResetAccountCredits] Consumed 1 bank-reset credit for %s — %d windows reset upstream",
		account.Email, windowsReset)
	msg := fmt.Sprintf("Bank reset credit consumed for %s — %d rate-limit windows reset", account.Email, windowsReset)
	if stillDisabled {
		msg += " (account is still disabled — enable it to put it back in the routing pool)"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"windowsReset":  windowsReset,
		"available":     available - 1,
		"stillDisabled": stillDisabled,
		"message":       msg,
	})
}

// apiReauthAllBanned POST /admin/api/accounts/reauth-all-banned
// Iterates every banned Codex account, refreshes its OAuth token, and
// re-fetches usage. Does NOT clear the ban — a banned account is only
// unbanned by a successful test request (apiTestAccount). This endpoint
// just refreshes tokens + usage so the operator can see fresh data and
// decide which accounts to test. Non-Codex banned accounts are skipped.
//
// After this call returns, the operator should press "Test" on each
// account; successful tests will clear the ban automatically.
func (h *Handler) apiReauthAllBanned(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	refreshed, failed, skipped := 0, 0, 0
	for i := range accounts {
		a := &accounts[i]
		if a.BanStatus == "" || a.BanStatus == "ACTIVE" {
			continue
		}
		if !isCodexAccount(a) {
			skipped++
			continue
		}
		// Try to refresh the OAuth token. The helper commits only after a
		// complete successful response, so a failed refresh leaves both token
		// values and the account status untouched.
		if a.RefreshToken != "" {
			if err := refreshCodexAccountToken(a); err != nil {
				failed++
				logger.Warnf("[apiReauthAllBanned] %s token refresh failed; account preserved: %v", a.Email, err)
				continue
			}
		}
		// Refresh profile + usage so the UI shows fresh data. Do NOT
		// clear ban — operator must press "Test" to verify recovery.
		refreshCodexAccountID(a)
		_ = h.fetchAndCacheAccountModels(a)
		if usageErr := fetchCodexUsage(a); usageErr != nil {
			logger.Warnf("[apiReauthAllBanned] %s usage fetch failed: %v", a.Email, usageErr)
		}
		_ = config.UpdateAccountPreservingCredentials(a.ID, *a)
		refreshed++
	}
	h.pool.Reload()
	logger.Infof("[apiReauthAllBanned] Bulk token+usage refresh complete — refreshed=%d failed=%d skipped=%d (ban NOT cleared; press Test to verify)",
		refreshed, failed, skipped)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": refreshed,
		"failed":    failed,
		"skipped":   skipped,
		"message":   fmt.Sprintf("Refreshed token+usage for %d accounts. Press Test on each to clear ban. Failed: %d, skipped: %d.", refreshed, failed, skipped),
	})
}

// apiRefreshAccountToken POST /admin/api/accounts/{id}/refresh-token
// Forces an OAuth refresh-token flow for the account, regardless of token
// expiry. Returns the new expiry + refresh timestamp so the admin UI can
// show "last refreshed" and the new countdown. Works for all account types
// that have a refresh_token (Kiro, Codex, external_idp).
func (h *Handler) apiRefreshAccountToken(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if isServiceAccount(account) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "service accounts use API keys and cannot refresh OAuth tokens"})
		return
	}
	if account.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account has no refresh token"})
		return
	}
	// get latest token from pool at runtime
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	newAccess, newRefresh, newExpires, profileArn, _, _, err := auth.RefreshAccountToken(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	if err := config.UpdateAccountToken(id, newAccess, newRefresh, newExpires); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not persist refreshed token: " + err.Error()})
		return
	}
	account.AccessToken = newAccess
	if newRefresh != "" {
		account.RefreshToken = newRefresh
	}
	account.ExpiresAt = newExpires
	h.pool.UpdateToken(id, newAccess, newRefresh, newExpires)
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(id, profileArn)
	}
	// For Codex accounts, re-extract chatgpt_account_id from the new JWT
	// since it may rotate when OpenAI re-issues the access token.
	if isCodexAccount(account) {
		refreshCodexAccountID(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":          true,
		"expiresAt":        newExpires,
		"tokenRefreshedAt": time.Now().Unix(),
	})
}

// apiRestoreCodexRefreshToken POST /admin/api/accounts/{id}/restore-refresh-token
// restores an existing Codex account from an operator-provided backup refresh
// token. The supplied token is never persisted until OpenAI returns a valid
// access token for the same ChatGPT account.
func (h *Handler) apiRestoreCodexRefreshToken(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.RefreshToken = strings.TrimSpace(req.RefreshToken)
	if req.RefreshToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Backup refresh token is required"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !isCodexAccount(account) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Only Codex accounts support backup refresh-token recovery"})
		return
	}
	if strings.TrimSpace(account.ChatGPTAccountID) == "" {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account has no ChatGPT account ID; backup token recovery cannot be verified safely"})
		return
	}

	// Refresh using a copy so a failed upstream request cannot alter the
	// existing account in memory or on disk.
	candidate := *account
	candidate.RefreshToken = req.RefreshToken
	newAccessToken, newRefreshToken, newExpiresAt, _, _, _, err := auth.RefreshAccountToken(&candidate)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "Backup token recovery failed: " + err.Error()})
		return
	}
	if strings.TrimSpace(newAccessToken) == "" || newExpiresAt <= time.Now().Unix() {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "Backup token recovery returned an invalid access token"})
		return
	}
	if newRefreshToken == "" {
		newRefreshToken = req.RefreshToken
	}

	newAccountID := auth.ExtractCodexAccountIDPublic(newAccessToken)
	if newAccountID == "" {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "Backup token recovery returned no ChatGPT account ID"})
		return
	}
	if account.ChatGPTAccountID != newAccountID {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Backup token belongs to a different ChatGPT account; no changes were saved"})
		return
	}

	// Persist the complete rotation before updating the pool. No token values
	// are included in logs or the response.
	if err := config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not persist recovered token: " + err.Error()})
		return
	}
	account.AccessToken = newAccessToken
	account.RefreshToken = newRefreshToken
	account.ExpiresAt = newExpiresAt
	h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
	refreshCodexAccountID(account)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":          true,
		"expiresAt":        newExpiresAt,
		"tokenRefreshedAt": time.Now().Unix(),
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.Provider == "" {
		base.Provider = extra.Provider
	}
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

// modelInfoTokenLimits resolves token limits for model discovery and CLI config.
// Claude 5/Fable 5 limits are canonical because stale cache entries for these
// models were the source of the incorrect context-window configuration. Other
// models retain catalog metadata as the authority, with policy values filling
// only missing fields.
func modelInfoTokenLimits(info ModelInfo) (int, int, bool) {
	policyInput, policyOutput, hasPolicy := policyModelLimits(info.ModelId)
	input, output := policyInput, policyOutput
	if info.TokenLimits != nil {
		if info.TokenLimits.MaxInputTokens > 0 {
			if !isCanonicalClaude5Model(info.ModelId) {
				input = info.TokenLimits.MaxInputTokens
			}
		}
		if info.TokenLimits.MaxOutputTokens > 0 {
			if !isCanonicalClaude5Model(info.ModelId) {
				output = info.TokenLimits.MaxOutputTokens
			}
		}
	}
	return input, output, input > 0 || output > 0 || hasPolicy
}

func isCanonicalClaude5Model(model string) bool {
	model, _ = ParseModelAndThinking(model, "-thinking")
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "claude-opus-5" || model == "claude-sonnet-5" || model == "claude-haiku-5"
}

func modelTokenLimitsFromCatalog(catalog []ModelInfo, model string) (int, int, bool) {
	for _, info := range catalog {
		if strings.EqualFold(strings.TrimSpace(info.ModelId), model) {
			return modelInfoTokenLimits(info)
		}
	}
	return 0, 0, false
}

// contextWindowForModel returns the real per-model input-token window reported
// by ListAvailableModels (maxInputTokens), used to convert the upstream
// contextUsagePercentage into an absolute input-token count. Falls back to the
// hard-coded getContextWindowSize heuristic when the model is not cached yet or
// upstream omitted its token limits. The model name is normalized (thinking
// suffix stripped) so "...-thinking" variants resolve to the base model.
func (h *Handler) contextWindowForModel(model string) int {
	base, _ := ParseModelAndThinking(model, "-thinking")
	baseLower := strings.ToLower(strings.TrimSpace(base))
	if input, _, ok := modelTokenLimitsFromCatalog(codexSubscriptionModels(), baseLower); ok && input > 0 {
		return input
	}

	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()

	if input, _, ok := modelTokenLimitsFromCatalog(cached, baseLower); ok && input > 0 {
		return input
	}
	return getContextWindowSize(model)
}

// modelTokenLimits resolves the limits already provided by the model catalog.
// Agent configuration uses the same metadata as runtime context accounting so
// selecting a model cannot leave stale limits from the previously selected one.
func (h *Handler) modelTokenLimits(model string) (int, int, bool) {
	model = strings.TrimSpace(model)
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		model = strings.TrimSpace(model[idx+1:])
	}
	base, _ := ParseModelAndThinking(model, "-thinking")
	baseLower := strings.ToLower(strings.TrimSpace(base))
	if baseLower == "" {
		return 0, 0, false
	}
	// Subscription model metadata is canonical. In particular, do not allow a
	// stale discovery-cache entry to downgrade Luna from 200K to 200 tokens.
	if input, output, ok := modelTokenLimitsFromCatalog(codexSubscriptionModels(), baseLower); ok {
		return input, output, true
	}
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if input, output, ok := modelTokenLimitsFromCatalog(cached, baseLower); ok {
		return input, output, true
	}
	if input, output, ok := policyModelLimits(baseLower); ok {
		return input, output, true
	}
	return 0, 0, false
}

// upsertYAMLAuxiliaryModels routes every model-backed Hermes auxiliary task
// through the same OmniProxy endpoint while retaining task-specific options
// such as timeout, extra_body, and download_timeout.
func upsertYAMLAuxiliaryModels(raw, baseURL, apiKey string) string {
	slots := []string{"vision", "web_extract", "compression", "skills_hub", "approval", "mcp", "title_generation", "triage_specifier", "kanban_decomposer", "profile_describer", "curator"}
	if strings.TrimSpace(raw) == "" {
		return "auxiliary:\n" + auxiliaryModelBlocks(baseURL, apiKey, slots)
	}

	lines := strings.Split(raw, "\n")
	auxIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "auxiliary:" && len(line)-len(strings.TrimLeft(line, " ")) == 0 {
			auxIdx = i
			break
		}
	}
	if auxIdx < 0 {
		return strings.TrimRight(raw, "\n") + "\nauxiliary:\n" + auxiliaryModelBlocks(baseURL, apiKey, slots)
	}

	// These are the model-backed auxiliary tasks in Hermes' DEFAULT_CONFIG.
	// web.search_backend and web.extract_backend remain adapters; only the
	// optional LLM call made by auxiliary.web_extract is routed here.
	for _, slot := range slots {
		start := -1
		for i := auxIdx + 1; i < len(lines); i++ {
			indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			trimmed := strings.TrimSpace(lines[i])
			if trimmed != "" && indent == 0 {
				break
			}
			if indent == 2 && trimmed == slot+":" {
				start = i
				break
			}
		}
		if start < 0 {
			insertAt := len(lines)
			for i := auxIdx + 1; i < len(lines); i++ {
				indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
				if strings.TrimSpace(lines[i]) != "" && indent == 0 {
					insertAt = i
					break
				}
			}
			block := strings.Split(strings.TrimRight(auxiliaryModelBlockForSlot(baseURL, apiKey, slot), "\n"), "\n")
			lines = append(lines[:insertAt], append(block, lines[insertAt:]...)...)
			continue
		}
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			if strings.TrimSpace(lines[i]) != "" && indent <= 2 {
				end = i
				break
			}
		}
		block := strings.Split(strings.TrimRight(auxiliaryModelBlockForSlot(baseURL, apiKey, slot), "\n"), "\n")
		// Preserve every existing option except the four routing keys. Keep
		// nested extra_body/provider values by filtering only direct slot keys.
		for _, line := range lines[start+1 : end] {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			trimmed := strings.TrimSpace(line)
			if indent == 4 && (strings.HasPrefix(trimmed, "provider:") || strings.HasPrefix(trimmed, "model:") || strings.HasPrefix(trimmed, "base_url:") || strings.HasPrefix(trimmed, "api_key:")) {
				continue
			}
			block = append(block, line)
		}
		lines = append(lines[:start], append(block, lines[end:]...)...)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

func auxiliaryModelBlockForSlot(baseURL, apiKey, slot string) string {
	return fmt.Sprintf("  %s:\n    provider: omniproxy\n    model: %s\n    base_url: %s\n    api_key: %s\n", slot, defaultAgentModel, yamlQuoteIfNeeded(baseURL), yamlQuoteIfNeeded(apiKey))
}

func auxiliaryModelBlocks(baseURL, apiKey string, slots []string) string {
	var out strings.Builder
	for _, slot := range slots {
		out.WriteString(auxiliaryModelBlockForSlot(baseURL, apiKey, slot))
	}
	return out.String()
}

func hermesModelID(model string) string {
	return openClawModelID(model)
}

func hermesProviderBlock(baseURL, apiKey string, catalog []ModelInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  omniproxy:\n    base_url: %s\n    api_key: %s\n    api_mode: openai\n    discover_models: false\n    models:\n", yamlQuoteIfNeeded(baseURL), yamlQuoteIfNeeded(apiKey))
	for _, info := range catalog {
		id := hermesModelID(info.ModelId)
		if id == "" {
			continue
		}
		input, output, ok := modelInfoTokenLimits(info)
		fmt.Fprintf(&b, "      %s:\n", yamlQuoteIfNeeded(id))
		if ok && input > 0 {
			fmt.Fprintf(&b, "        context_length: %d\n", input)
		}
		if ok && output > 0 {
			fmt.Fprintf(&b, "        max_tokens: %d\n", output)
		}
	}
	return b.String()
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens counts tokens (called by Claude Code)
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages handles Claude API
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// read request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// Strip provider prefix (e.g. "codezdev/claude-opus-4-8" → "claude-opus-4-8")
	// so the request routes to the same pool entries as the bare model name.
	req.Model = stripProviderPrefix(req.Model)

	// Check if model is a combo name FIRST, before thinking/alias resolution.
	// This prevents alias mappings (e.g. "gpt-4o" → "claude-sonnet-4.5") from
	// defeating combo detection when a combo shares an alias name.
	// Skip combo resolution for sub-requests dispatched by the combo handler itself
	// (prevents infinite recursion when a combo model shares the combo name).
	if r.Context().Value(comboBypassKey) == nil {
		if comboName, comboModels, ok := resolveComboModels(req.Model); ok {
			body, _ := json.Marshal(req)
			h.handleComboRequest(w, r, comboName, comboModels, body, "claude")
			return
		}
	}

	// parse model and thinking mode
	thinkingCfg := config.GetThinkingConfig()
	originalModel := stripThinkingSuffix(req.Model, thinkingCfg.Suffix)
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// transform request
	kiroPayload := ClaudeToKiro(&req, thinking)
	kiroPayload.OriginalModel = originalModel

	// Stream or non-stream
	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// handleClaudeStream handles Claude streaming response
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// get thinking output format config
	thinkingFormat := thinkingOpts.Format

	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	// lastAccountID remembers the account that produced lastErr so the final
	// recordError call can attribute the failure to a concrete account. Without
	// it every failed request landed in Usage with an empty account column and
	// provider "unknown", making errors untraceable in the dashboard.
	var lastAccountID string
	messageStarted := false
	// Upstream cache values are populated only after the provider emits usage.
	// The message_start event therefore must not expose the local estimate.
	realCacheRead := 0
	realCacheCreate := 0

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, promptCacheUsage{}, false),
			},
		})
		messageStarted = true
	}

	cacheKey := payloadCacheKey(payload)
	for attempt := 0; ; attempt++ {
		logger.Warnf("[CLAUDE-STREAM] model=%s attempt=%d pool_accounts=%d excluded=%v",
			model, attempt, h.pool.Count(), excluded)
		account := h.pool.GetNextForModelWithCacheKey(model, excluded, cacheKey)
		if account == nil {
			logger.Warnf("[CLAUDE-STREAM] model=%s no account found after %d attempts",
				model, attempt)
			break
		}
		h.logCacheRouting("claude-stream", model, cacheKey, payload, account)
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
		// Skip the simulated cache tracker for External OpenAI-compatible
		// providers — they report real cached_tokens via OnCacheRead, which
		// overrides cacheUsage below. Running Compute/Update here would only
		// add mutex contention and synthetic fingerprints for nothing.
		var cacheUsage promptCacheUsage
		if !isExternalAccount(account) {
			cacheUsage = h.promptCache.Compute(account.ID, cacheProfile)
		}
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		stopReason := "end_turn"
		// This tracks output from the upstream callback before any downstream
		// coalescing. Once output has escaped the upstream, retrying would replay
		// the prefix and make claude-cli request a manual continue.
		upstreamProduced := false
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		activeBlockIndex := -1
		activeBlockType := ""
		realCacheRead = 0 // reset per-attempt; only the successful attempt's count is used
		realCacheCreate = 0

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			// Thinking blocks require a signature_delta before content_block_stop.
			// Without it, claude-cli rejects the thinking block and drops subsequent
			// tool_use blocks (root cause of 40% tool_use mismatch with gpt-5.6-sol).
			// Non-Claude models can't produce real signatures, so we emit a stable
			// placeholder — claude-cli only needs the field present, it does not
			// verify the signature client-side.
			if activeBlockType == "thinking" {
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{
						"type":      "signature_delta",
						"signature": "EqoBCkgIARABGAIiIL2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d",
					},
				})
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		var visibleTextStarted bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				visibleTextStarted = true
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if visibleTextStarted {
					// For non-Claude models (e.g. gpt-5.6-sol), reasoning often
					// arrives AFTER visible text in the OpenAI stream. Dropping it
					// loses context. Convert late reasoning to text instead.
					// Claude-native models enforce thinking-before-text ordering.
					if !strings.HasPrefix(strings.ToLower(model), "claude-") {
						sendText(text, 0)
					}
					return
				}
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					// Detect both <thinking> (long) and <think> (short) open tags.
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					thinkStart := strings.Index(textBuffer, "<think>")
					var openPos, openTagLen int
					if thinkingStart != -1 && (thinkStart == -1 || thinkingStart < thinkStart) {
						openPos, openTagLen = thinkingStart, 10
					} else if thinkStart != -1 {
						openPos, openTagLen = thinkStart, 6
					} else {
						openPos, openTagLen = -1, 0
					}
					if openPos != -1 {
						if openPos > 0 {
							sendText(textBuffer[:openPos], 0)
						}
						textBuffer = textBuffer[openPos+openTagLen:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					// Detect both </thinking> (long) and </think> (short) close tags.
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					thinkEnd := strings.Index(textBuffer, "</think>")
					var closePos, closeTagLen int
					if thinkingEnd != -1 && (thinkEnd == -1 || thinkingEnd < thinkEnd) {
						closePos, closeTagLen = thinkingEnd, 11
					} else if thinkEnd != -1 {
						closePos, closeTagLen = thinkEnd, 7
					} else {
						closePos, closeTagLen = -1, 0
					}
					if closePos != -1 {
						content := textBuffer[:closePos]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[closePos+closeTagLen:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnOutput: func() {
				upstreamProduced = true
			},
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				upstreamProduced = true
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				upstreamProduced = true
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnStopReason: func(reason string) {
				if reason != "" {
					stopReason = reason
				}
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnCacheRead: func(cachedTokens int) {
				if cachedTokens > realCacheRead {
					realCacheRead = cachedTokens
				}
			},
			OnCacheCreate: func(cacheCreateTokens int) {
				if cacheCreateTokens > realCacheCreate {
					realCacheCreate = cacheCreateTokens
				}
			},
		}

		h.usageTracker.TrackActive(account.ID, endpointClaude, model)
		// Codex accounts emit per-token deltas from /v1/responses; wrap the
		// callback with downstream coalescing so we don't json.Marshal +
		// Flush per token (cuts syscall count ~50-100x for reasoning models).
		effectiveCallback := callback
		if isCodexAccount(account) {
			effectiveCallback = newCodexCoalescer(callback)
		}
		err := dispatchChat(account, payload, effectiveCallback)
		if err != nil {
			lastErr = err
			if upstreamProduced {
				// The response is already visible to the client. Retrying would
				// replay the prefix, so terminate this SSE turn in place. The
				// coalescer may still hold the last tokens and must be flushed
				// before the error event is sent.
				if effectiveCallback.OnError != nil {
					effectiveCallback.OnError(err)
				}
				h.usageTracker.RemoveActive(account.ID)
				processClaudeText("", false, true)
				if eventThinkingOpen {
					sendText("", 3)
				}
				closeActiveBlock()
				ensureMessageStart()
				h.recordFailure()
				h.sendSSE(w, flusher, "error", map[string]interface{}{
					"type":  "error",
					"error": map[string]string{"type": "api_error", "message": err.Error()},
				})
				return
			}
			// Transient upstream errors (5xx, overload, timeout) are retried
			// in-place with backoff before rotating to a different account.
			if h.tryTransientRetry(account, payload, effectiveCallback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto skipAccountHandling
			}
			//  try refresh+retry before rotating accounts
			if h.tryRefreshAndRetry(account, payload, effectiveCallback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				// Retry succeeded, proceed to success path
				goto skipAccountHandling
			}
			if pool.IsContentBlockedError(err) {
				lastErr = err
				logger.Warnf("[ContentBlocked] %s: upstream refused payload for model %s — skipping account (err: %s)", account.Email, model, truncateForLog(err.Error()))
				excluded[account.ID] = true
				continue
			}
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			if !messageStarted {
				continue
			}
			h.recordFailure()
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			return
		}
	skipAccountHandling:

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		// Input precedence: exact upstream count (OnComplete) > contextUsage-derived
		// (pct × real window) > pre-request estimate. inputTokens already holds the
		// OnComplete value here; only fall back when upstream gave us nothing.
		if inputTokens <= 0 {
			if realInputTokens > 0 {
				inputTokens = realInputTokens
			} else {
				inputTokens = estimatedInputTokens
			}
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		// Output: prefer the exact upstream count; estimate only when absent.
		if outputTokens <= 0 {
			outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)
		}

		upstreamCacheUsage := promptCacheUsage{
			CacheReadInputTokens:     realCacheRead,
			CacheCreationInputTokens: realCacheCreate,
		}
		h.recordUsageWithCache(apiKeyID, account.ID, model, endpointClaude, inputTokens, outputTokens, credits, cacheUsageTelemetry{
			ReadTokens:            realCacheRead,
			CreateTokens:          realCacheCreate,
			EstimatedReadTokens:   cacheUsage.CacheReadInputTokens,
			EstimatedCreateTokens: cacheUsage.CacheCreationInputTokens,
		})
		h.pool.RecordSuccess(account.ID, model)
		if cacheKey != "" {
			h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
		}
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		// Keep the tracker as a prediction/affinity aid. Its values are never
		// used as client-facing or billed cache usage.
		h.promptCache.Update(account.ID, cacheProfile)

		if len(toolUses) > 0 && stopReason == "end_turn" {
			stopReason = "tool_use"
		}

		ensureMessageStart()
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, upstreamCacheUsage, realCacheRead > 0 || realCacheCreate > 0),
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.sendClaudeSSEError(w, flusher, "api_error", "No available accounts")
		return
	}

	h.recordError(apiKeyID, lastAccountID, model, endpointClaude, lastErr.Error())
	h.sendClaudeSSEError(w, flusher, "api_error", lastErr.Error())
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// sendClaudeSSEError sends an error as a proper Claude SSE error event.
// Use this instead of sendClaudeError when the response is already in SSE mode
// (Content-Type: text/event-stream has been committed). Sending a JSON error
// body after SSE headers are set produces malformed output that causes
// streaming clients (e.g. Claude Code CLI) to hang silently.
func (h *Handler) sendClaudeSSEError(w http.ResponseWriter, flusher http.Flusher, errType, message string) {
	h.sendSSE(w, flusher, "error", map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// sendOpenAISSEError sends an error as an OpenAI SSE data event followed by [DONE].
// Use this instead of sendOpenAIError when the response is already in SSE mode.
func (h *Handler) sendOpenAISSEError(w http.ResponseWriter, flusher http.Flusher, errType, message string) {
	data, _ := json.Marshal(map[string]interface{}{
		"error": map[string]string{
			"message": message,
			"type":    errType,
		},
	})
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// backgroundStatsSaver periodically saves stats
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // save once before exit
			return
		}
	}
}

// saveStats saves stats to config file
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// getCredits thread-safely gets credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits thread-safely adds credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// stats tracking (using atomic operations)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// recordUsage records a successful request. It updates BOTH accounting systems:
// the global atomics + per-API-key counters (via recordSuccessForApiKey, surfaced
// at /status and /stats and the By-API-key table) and the time-series usage
// tracker (surfaced at /usage/stats). recordSuccessForApiKey runs first and
// outside the tracker-nil guard so the global success/token/credit totals advance
// on every real request — previously nothing called it, so only recordFailure ran
// and totalRequests counted failures only while successRequests stayed frozen.
// payloadCacheKey derives a stable, opaque cache-routing key from the
// payload's system prompt (instructions). All conversations using the
// same instructions share the same cache key, so they benefit from the
// same warmed cache entry on each account. Returns empty when the
// payload has no system prompt (cache won't work without instructions).
func payloadCacheKey(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	if instr := payloadCodexInstructions(payload); instr != "" {
		return codexCacheKey(instr)
	}
	// No system prompt: fall back to the conversation id instead of giving up on
	// affinity entirely.
	//
	// The previous behaviour returned an empty key here, which switched routing
	// back to plain rotation for the whole conversation. That was based on the
	// belief that the upstream only caches the instructions field — but OpenAI's
	// prompt-caching documentation states the rendered prefix includes system,
	// developer, user and assistant messages, tool definitions and images, so a
	// conversation with no system prompt still has a large cacheable prefix.
	// Measured traffic showed this path was not hypothetical: the majority of
	// gpt-5.6-sol requests arrived without a system message and therefore ran
	// with no affinity at all, on the most expensive model in the pool.
	return codexCacheKey(payloadConversationPrefix(payload))
}

// payloadConversationPrefix returns a stable identifier for a conversation,
// used as a cache-routing key when there is no system prompt.
//
// It keys on ConversationID rather than on the first history turn. Keying on
// history content looks equivalent but is not: truncatePayloadToLimit drops the
// oldest turns once a payload outgrows the size limit, and with no system prompt
// there is no priming pair to protect the head of the history — so the "first"
// turn changes mid-conversation and the key drifts with it. That was observed in
// live traffic, where a single conversation produced three different keys within
// two minutes, repinning to a fresh account each time.
//
// ConversationID is computed by the translators before truncation and stays
// fixed for the life of a conversation, which is exactly the property a routing
// key needs. Returns empty when there is no history yet (a genuinely first
// request, where rotation is correct) so that single-shot traffic does not
// accumulate one sticky entry per request.
func payloadConversationPrefix(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	if len(payload.ConversationState.History) == 0 {
		return ""
	}
	convID := strings.TrimSpace(payload.ConversationState.ConversationID)
	if convID == "" {
		return ""
	}
	return "conv-id:" + convID
}

// shortHash abbreviates an opaque routing hash for logs. Diagnostics only need
// enough characters to tell two keys apart across consecutive turns, and a full
// 32-char hash per line makes the log unreadable at request volume.
func shortHash(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// logCacheRouting emits one line describing the prompt-cache routing decision
// for a request, gated behind the cacheDiagnostics setting.
//
// The three fields together separate the two failure modes that the usage store
// cannot distinguish. ckey is the prefix hash the pool pins on and the Codex
// path sends as prompt_cache_key; conv is the hash driving the session-id /
// thread-id headers that decide upstream machine affinity. A turn that keeps
// the same ckey but changes conv lost machine affinity; a turn whose ckey
// changes lost the pin itself. sticky reports whether the pin existed and was
// honoured, which distinguishes "no pin yet" from "pin present but the account
// was unavailable and rotation took over".
//
// Only hashes, ids and counters are logged — never instructions or message
// content — so the line stays safe to keep in a shared log.
func (h *Handler) logCacheRouting(tag, model, cacheKey string, payload *KiroPayload, account *config.Account) {
	if !config.GetCacheDiagnostics() {
		return
	}
	convID := ""
	if payload != nil {
		convID = strings.TrimSpace(payload.ConversationState.ConversationID)
	}
	sticky := "none"
	if cacheKey != "" && h.pool != nil {
		if pinned, live := h.pool.PeekCacheSticky(model, cacheKey); pinned != "" {
			switch {
			case !live:
				sticky = "expired"
			case account != nil && pinned == account.ID:
				sticky = "hit"
			default:
				sticky = "diverted"
			}
		}
	}
	name := "-"
	if account != nil {
		name = account.Email
	}
	logger.Warnf("[CACHE-DIAG] %s model=%s ckey=%s conv=%s sticky=%s account=%s",
		tag, model, shortHash(cacheKey), shortHash(convID), sticky, name)
}

// payloadCodexInstructions extracts the system prompt that the Codex
// translator would place in the "instructions" field of the Responses API
// request. This is the prefix that gets cached by the ChatGPT backend.
// Returns empty when there is no system prompt (cache won't work in that
// case — the backend only caches the instructions field, not input content).
func payloadCodexInstructions(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	history := payload.ConversationState.History
	if len(history) >= 2 {
		first := history[0]
		second := history[1]
		if first.UserInputMessage != nil && second.AssistantResponseMessage != nil &&
			strings.Contains(strings.ToLower(strings.TrimSpace(second.AssistantResponseMessage.Content)), "i will follow") {
			return strings.TrimSpace(first.UserInputMessage.Content)
		}
	}
	// Leading user-only system prompt (non-Claude clients).
	if len(history) > 0 && history[0].UserInputMessage != nil {
		c := strings.TrimSpace(history[0].UserInputMessage.Content)
		if strings.HasPrefix(c, "You are ") {
			return c
		}
	}
	return ""
}

type cacheUsageTelemetry struct {
	ReadTokens            int
	CreateTokens          int
	CachedTokens          int
	EstimatedReadTokens   int
	EstimatedCreateTokens int
}

// recordUsage keeps the legacy call shape for non-chat service endpoints.
// Chat handlers should use recordUsageWithCache so upstream cache telemetry is
// explicitly separated from local estimates.
func (h *Handler) recordUsage(apiKeyID, accountID, model, endpoint string, inputTokens, outputTokens int, credits float64, cacheRead, cacheCreate, cachedTokens int) {
	h.recordUsageWithCache(apiKeyID, accountID, model, endpoint, inputTokens, outputTokens, credits, cacheUsageTelemetry{
		ReadTokens:   cacheRead,
		CreateTokens: cacheCreate,
		CachedTokens: cachedTokens,
	})
}

func (h *Handler) recordUsageWithCache(apiKeyID, accountID, model, endpoint string, inputTokens, outputTokens int, credits float64, cache cacheUsageTelemetry) {
	h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
	if h.usageTracker == nil {
		return
	}
	provider, accountName := resolveAccountMeta(accountID)
	rec := RequestRecord{
		Model:                      model,
		Provider:                   provider,
		AccountID:                  accountID,
		AccountName:                accountName,
		InputTokens:                inputTokens,
		OutputTokens:               outputTokens,
		Cost:                       credits,
		Status:                     statusSuccess,
		Endpoint:                   endpoint,
		APIKeyID:                   apiKeyID,
		CacheReadTokens:            maxInt(cache.ReadTokens, 0),
		CacheCreateTokens:          maxInt(cache.CreateTokens, 0),
		CachedTokens:               maxInt(cache.CachedTokens, 0),
		EstimatedCacheReadTokens:   maxInt(cache.EstimatedReadTokens, 0),
		EstimatedCacheCreateTokens: maxInt(cache.EstimatedCreateTokens, 0),
	}
	if rec.CacheReadTokens > 0 || rec.CacheCreateTokens > 0 || rec.CachedTokens > 0 {
		rec.CacheSource = "upstream"
	} else if rec.EstimatedCacheReadTokens > 0 || rec.EstimatedCacheCreateTokens > 0 {
		rec.CacheSource = "estimated"
	}
	h.usageTracker.Append(rec)
}

// resolveAccountMeta looks up the upstream provider (BuilderId/ExternalIdp) and a
// display name for an account ID, for tagging usage records. Returns "unknown"
// provider and empty name when the account is not found or ID is empty.
func resolveAccountMeta(accountID string) (provider, accountName string) {
	provider = "unknown"
	if accountID == "" {
		return provider, ""
	}
	for _, a := range config.GetAccounts() {
		if a.ID != accountID {
			continue
		}
		if a.Provider != "" {
			provider = a.Provider
		}
		if a.Nickname != "" {
			accountName = a.Nickname
		} else if a.Email != "" {
			accountName = a.Email
		} else if len(a.ID) >= 8 {
			accountName = a.ID[:8]
		} else {
			accountName = a.ID
		}
		break
	}
	return provider, accountName
}

// recordError records a FAILED request: it bumps the global failure counters
// (like the old recordFailure) AND appends a RequestRecord with Status=statusError
// and the error message, so failed requests are visible in Usage → Recent Requests
// and in the By-* breakdowns with their reason — previously recordFailure only bumped
// a counter and the failed request vanished from every table.
func (h *Handler) recordError(apiKeyID, accountID, model, endpoint, errMsg string) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
	if apiKeyID != "" {
		if err := config.RecordApiKeyError(apiKeyID, errMsg); err != nil {
			logger.Warnf("[ApiKey] failed to record error for key %s: %v", apiKeyID, err)
		}
	}
	if h.usageTracker == nil {
		return
	}
	provider, accountName := resolveAccountMeta(accountID)
	h.usageTracker.Append(RequestRecord{
		Model:       model,
		Provider:    provider,
		AccountID:   accountID,
		AccountName: accountName,
		Status:      statusError,
		Endpoint:    endpoint,
		APIKeyID:    apiKeyID,
		Error:       errMsg,
	})
}

// handleClaudeNonStream handles Claude non-streaming response
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error
	// lastAccountID attributes the final failure to a concrete account in Usage.
	var lastAccountID string
	cacheKey := payloadCacheKey(payload)

	for attempt := 0; ; attempt++ {
		account := h.pool.GetNextForModelWithCacheKey(model, excluded, cacheKey)
		if account == nil {
			break
		}
		h.logCacheRouting("claude-nonstream", model, cacheKey, payload, account)
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
		// Skip simulated cache for External OpenAI providers — they report
		// real cached_tokens via OnCacheRead which overrides cacheUsage.
		var cacheUsage promptCacheUsage
		if !isExternalAccount(account) {
			cacheUsage = h.promptCache.Compute(account.ID, cacheProfile)
		}

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		realCacheRead := 0
		realCacheCreate := 0
		attemptProduced := false
		resetAttempt := func() {
			content = ""
			thinkingContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			realCacheRead = 0
			realCacheCreate = 0
			attemptProduced = false
		}

		callback := &KiroStreamCallback{
			OnOutput:  func() { attemptProduced = true },
			HasOutput: func() bool { return attemptProduced },
			OnReset:   resetAttempt,
			OnText: func(text string, isThinking bool) {
				attemptProduced = true
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				attemptProduced = true
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnCacheRead: func(cachedTokens int) {
				if cachedTokens > realCacheRead {
					realCacheRead = cachedTokens
				}
			},
			OnCacheCreate: func(cacheCreateTokens int) {
				if cacheCreateTokens > realCacheCreate {
					realCacheCreate = cacheCreateTokens
				}
			},
		}

		h.usageTracker.TrackActive(account.ID, endpointClaude, model)
		err := dispatchChat(account, payload, callback)
		if err != nil {
			lastErr = err
			// Transient upstream errors (5xx, overload, timeout) are retried
			// in-place with backoff before rotating to a different account.
			if h.tryTransientRetry(account, payload, callback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto skipNonStreamHandling
			}
			// try refresh+retry before rotating accounts
			if h.tryRefreshAndRetry(account, payload, callback, err) {
				goto skipNonStreamHandling
			}
			if pool.IsContentBlockedError(err) {
				lastErr = err
				logger.Warnf("[ContentBlocked] %s: upstream refused payload for model %s — skipping account (err: %s)", account.Email, model, truncateForLog(err.Error()))
				excluded[account.ID] = true
				continue
			}
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
	skipNonStreamHandling:

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		// Input precedence: exact upstream count (OnComplete) > pct×window
		// reconstruction > pre-request estimate. The upstream count, when the
		// stream provides it, is exact; the pct×window value quantizes to ~1% of
		// the window so it is only a fallback.
		if inputTokens <= 0 {
			if realInputTokens > 0 {
				inputTokens = realInputTokens
			} else {
				inputTokens = estimatedInputTokens
			}
		}
		// Output: trust the upstream count when present; only estimate as fallback.
		if outputTokens <= 0 {
			outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)
		}

		upstreamCacheUsage := promptCacheUsage{
			CacheReadInputTokens:     realCacheRead,
			CacheCreationInputTokens: realCacheCreate,
		}
		h.recordUsageWithCache(apiKeyID, account.ID, model, endpointClaude, inputTokens, outputTokens, credits, cacheUsageTelemetry{
			ReadTokens:            realCacheRead,
			CreateTokens:          realCacheCreate,
			EstimatedReadTokens:   cacheUsage.CacheReadInputTokens,
			EstimatedCreateTokens: cacheUsage.CacheCreationInputTokens,
		})
		h.pool.RecordSuccess(account.ID, model)
		if cacheKey != "" {
			h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
		}
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, upstreamCacheUsage)
		resp.Usage.CacheCreationInputTokens = upstreamCacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = upstreamCacheUsage.CacheReadInputTokens
		if realCacheRead > 0 || realCacheCreate > 0 {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: upstreamCacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: upstreamCacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordError(apiKeyID, lastAccountID, model, endpointClaude, lastErr.Error())
	h.sendClaudeError(w, upstreamErrorStatus(lastErr), "api_error", lastErr.Error())
}

// upstreamErrorStatus maps a final upstream error to the HTTP status the proxy
// should surface to downstream clients. Transient/overload-class failures
// (upstream 502/503/504, "overloaded", "temporarily", rate-limit/quota) return
// 503 so downstream failover layers (e.g. OpenClaw) classify them as
// retryable/overloaded and advance to the next fallback model instead of
// treating the wrapped 500 as a hard timeout. Non-transient errors stay 500.
func upstreamErrorStatus(err error) int {
	if err == nil {
		return 500
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "http 502"),
		strings.Contains(lower, "http 503"),
		strings.Contains(lower, "http 504"),
		strings.Contains(lower, "overloaded"),
		strings.Contains(lower, "system cpu"),
		strings.Contains(lower, "temporarily"),
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "quota"),
		strings.Contains(lower, "too many requests"),
		strings.Contains(lower, "service unavailable"),
		strings.Contains(lower, "bad gateway"),
		strings.Contains(lower, "gateway timeout"):
		return 503
	default:
		return 500
	}
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat handles OpenAI API
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// Strip provider prefix (e.g. "codex/gpt-5.6-sol" → "gpt-5.6-sol")
	req.Model = stripProviderPrefix(req.Model)

	// Check if model is a combo name FIRST, before thinking/alias resolution.
	// Skip combo resolution for sub-requests dispatched by the combo handler itself
	// (prevents infinite recursion when a combo model shares the combo name).
	if r.Context().Value(comboBypassKey) == nil {
		if comboName, comboModels, ok := resolveComboModels(req.Model); ok {
			body, _ := json.Marshal(req)
			h.handleComboRequest(w, r, comboName, comboModels, body, "openai")
			return
		}
	}

	// parse model and thinking mode
	thinkingCfg := config.GetThinkingConfig()
	originalModel := stripThinkingSuffix(req.Model, thinkingCfg.Suffix)
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)
	kiroPayload.OriginalModel = originalModel

	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleOpenAIStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	} else {
		h.handleOpenAINonStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	}
}

// handleOpenAIStream handles OpenAI streaming response
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// get thinking output format config
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	excluded := make(map[string]bool)
	var lastErr error
	// lastAccountID attributes the final failure to a concrete account in Usage.
	var lastAccountID string
	// realCacheRead captures the upstream-reported prompt-cache hit count
	// (OpenAI prompt_tokens_details.cached_tokens or Kiro cacheReadInputTokens).
	// When > 0 it is reported to the client via prompt_tokens_details.cached_tokens
	// in the terminal usage chunk so the client sees the real cache behaviour.
	realCacheRead := 0
	realCacheCreate := 0

	cacheKey := payloadCacheKey(payload)
	for attempt := 0; ; attempt++ {
		logger.Warnf("[OPENAI-STREAM] model=%s attempt=%d pool_accounts=%d excluded=%v",
			model, attempt, h.pool.Count(), excluded)
		account := h.pool.GetNextForModelWithCacheKey(model, excluded, cacheKey)
		if account == nil {
			logger.Warnf("[OPENAI-STREAM] model=%s no account found after %d attempts",
				model, attempt)
			break
		}
		h.logCacheRouting("openai-stream", model, cacheKey, payload, account)
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false
		realCacheRead = 0 // reset per-attempt
		realCacheCreate = 0

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnOutput: func() { responseStarted = true },
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				responseStarted = true
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				responseStarted = true
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnCacheRead: func(cachedTokens int) {
				if cachedTokens > realCacheRead {
					realCacheRead = cachedTokens
				}
			},
			OnCacheCreate: func(cacheCreateTokens int) {
				if cacheCreateTokens > realCacheCreate {
					realCacheCreate = cacheCreateTokens
				}
			},
		}

		h.usageTracker.TrackActive(account.ID, endpointOpenAI, model)
		// Codex accounts: wrap callback with downstream coalescing to cut
		// per-token json.Marshal + Flush syscalls ~50-100x.
		effectiveCallback := callback
		if isCodexAccount(account) {
			effectiveCallback = newCodexCoalescer(callback)
		}
		err := dispatchChat(account, payload, effectiveCallback)
		if err != nil {
			lastErr = err
			// Transient upstream errors (5xx, overload, timeout) are retried
			// in-place with backoff before rotating to a different account.
			// A stream cannot be retried once any output has been produced. The
			// client may already have received the prefix (or the Codex
			// coalescer may still hold it), so retrying would replay that prefix
			// and leave Claude CLI waiting for a continuation.
			if !responseStarted && h.tryTransientRetry(account, payload, effectiveCallback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto skipOpenAIStreamHandling
			}
			//  try refresh+retry before rotating accounts
			if !responseStarted && h.tryRefreshAndRetry(account, payload, effectiveCallback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto skipOpenAIStreamHandling
			}
			if pool.IsContentBlockedError(err) {
				lastErr = err
				logger.Warnf("[ContentBlocked] %s: upstream refused payload for model %s — skipping account (err: %s)", account.Email, model, truncateForLog(err.Error()))
				excluded[account.ID] = true
				continue
			}
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			if !responseStarted {
				continue
			}
			h.recordFailure()
			errorData, _ := json.Marshal(map[string]interface{}{
				"error": map[string]string{
					"type":    "api_error",
					"message": err.Error(),
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", string(errorData))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
	skipOpenAIStreamHandling:

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		// Input precedence: exact upstream count (OnComplete) > pct×real-window
		// derivation > pre-request estimate. The percentage path quantizes to ~1%
		// of the window (10k tokens at 1M), so it must not override an exact count.
		if inputTokens <= 0 {
			if realInputTokens > 0 {
				inputTokens = realInputTokens
			} else {
				inputTokens = estimatedInputTokens
			}
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		// Output: prefer the exact upstream count; estimate only when absent.
		if outputTokens <= 0 {
			outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
			for _, tc := range toolCalls {
				outputTokens += estimateApproxTokens(tc.Function.Name)
				outputTokens += estimateApproxTokens(tc.Function.Arguments)
			}
		}

		h.recordUsageWithCache(apiKeyID, account.ID, model, endpointOpenAI, inputTokens, outputTokens, credits, cacheUsageTelemetry{
			CreateTokens: realCacheCreate,
			CachedTokens: realCacheRead,
		})
		h.pool.RecordSuccess(account.ID, model)
		if cacheKey != "" {
			h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
		}
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		// Build usage chunk. When the upstream reported a real prompt-cache
		// hit, surface it via prompt_tokens_details.cached_tokens so the
		// client (and any billing/observability layer) sees the real cache
		// behaviour of the upstream provider.
		usageMap := map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		}
		if realCacheRead > 0 {
			usageMap["prompt_tokens_details"] = map[string]int{
				"cached_tokens": realCacheRead,
			}
		}

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": usageMap,
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.sendOpenAISSEError(w, flusher, "server_error", "No available accounts")
		return
	}

	h.recordError(apiKeyID, lastAccountID, model, endpointOpenAI, lastErr.Error())
	h.sendOpenAISSEError(w, flusher, "server_error", lastErr.Error())
}

// handleOpenAINonStream handles OpenAI non-streaming response
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error
	// lastAccountID attributes the final failure to a concrete account in Usage.
	var lastAccountID string
	cacheKey := payloadCacheKey(payload)

	for attempt := 0; ; attempt++ {
		account := h.pool.GetNextForModelWithCacheKey(model, excluded, cacheKey)
		if account == nil {
			break
		}
		h.logCacheRouting("openai-nonstream", model, cacheKey, payload, account)
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		realCacheRead := 0
		realCacheCreate := 0
		attemptProduced := false
		resetAttempt := func() {
			content = ""
			reasoningContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			realCacheRead = 0
			realCacheCreate = 0
			attemptProduced = false
		}

		callback := &KiroStreamCallback{
			OnOutput:  func() { attemptProduced = true },
			HasOutput: func() bool { return attemptProduced },
			OnReset:   resetAttempt,
			OnText: func(text string, isThinking bool) {
				attemptProduced = true
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				attemptProduced = true
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnCacheRead: func(cachedTokens int) {
				if cachedTokens > realCacheRead {
					realCacheRead = cachedTokens
				}
			},
			OnCacheCreate: func(cacheCreateTokens int) {
				if cacheCreateTokens > realCacheCreate {
					realCacheCreate = cacheCreateTokens
				}
			},
		}

		h.usageTracker.TrackActive(account.ID, endpointOpenAI, model)
		err := dispatchChat(account, payload, callback)
		if err != nil {
			lastErr = err
			// Transient upstream errors (5xx, overload, timeout) are retried
			// in-place with backoff before rotating to a different account.
			if h.tryTransientRetry(account, payload, callback, err) {
				h.pool.RecordSuccess(account.ID, model)
				if cacheKey != "" {
					h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
				}
				goto skipOpenAINonStreamHandling
			}
			// try refresh+retry before rotating accounts
			if h.tryRefreshAndRetry(account, payload, callback, err) {
				goto skipOpenAINonStreamHandling
			}
			if pool.IsContentBlockedError(err) {
				lastErr = err
				logger.Warnf("[ContentBlocked] %s: upstream refused payload for model %s — skipping account (err: %s)", account.Email, model, truncateForLog(err.Error()))
				excluded[account.ID] = true
				continue
			}
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
	skipOpenAINonStreamHandling:

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		// Input precedence: exact upstream count (OnComplete) > pct×real-window
		// derivation > pre-request estimate. Output: trust the upstream count when
		// present, only estimate as a fallback.
		if inputTokens <= 0 {
			if realInputTokens > 0 {
				inputTokens = realInputTokens
			} else {
				inputTokens = estimatedInputTokens
			}
		}
		if outputTokens <= 0 {
			outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)
		}

		h.recordUsageWithCache(apiKeyID, account.ID, model, endpointOpenAI, inputTokens, outputTokens, credits, cacheUsageTelemetry{
			CreateTokens: realCacheCreate,
			CachedTokens: realCacheRead,
		})
		h.pool.RecordSuccess(account.ID, model)
		if cacheKey != "" {
			h.pool.RecordCacheStickiness(model, cacheKey, account.ID)
		}
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat, realCacheRead)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordError(apiKeyID, lastAccountID, model, endpointOpenAI, lastErr.Error())
	h.sendOpenAIError(w, upstreamErrorStatus(lastErr), "server_error", lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// tryRefreshAndRetry attempts to refresh the account token and retry the API call
// on 401/403 auth errors. Returns true if the retry succeeded, false otherwise.
// Call this from the retry loops BEFORE marking the account as failed.
func (h *Handler) tryRefreshAndRetry(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback, err error) bool {
	if err == nil {
		return false
	}
	// External OpenAI-compatible providers have no refresh token; a 401 means
	// the configured API key is invalid. Don't attempt Kiro-style refresh.
	// Codex accounts DO have an OAuth refresh token, so they fall through.
	if isExternalAccount(account) {
		return false
	}
	if !pool.IsAuthFailure(err) {
		return false
	}
	logger.Warnf("[AuthRetry] Auth failure for %s, attempting token refresh + retry", account.Email)

	// Serialize refresh + persistence for this handler. A request may hold an
	// older account snapshot while another request has already rotated the
	// refresh token; use the pool's latest credentials before attempting OAuth.
	h.tokenRefreshMu.Lock()
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	oldRefreshToken := account.RefreshToken
	newAccessToken, newRefreshToken, newExpiresAt, profileArn, _, _, refreshErr := auth.RefreshAccountToken(account)
	if refreshErr != nil || newAccessToken == "" {
		h.tokenRefreshMu.Unlock()
		logger.Warnf("[AuthRetry] Token refresh failed for %s: %v", account.Email, refreshErr)
		return false
	}
	if newRefreshToken == "" {
		newRefreshToken = oldRefreshToken
	}
	if err := config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt); err != nil {
		h.tokenRefreshMu.Unlock()
		logger.Warnf("[AuthRetry] Failed to persist refreshed token for %s: %v", account.Email, err)
		return false
	}
	auth.RecordRotation(oldRefreshToken, newAccessToken, newRefreshToken, profileArn, newExpiresAt)
	account.AccessToken = newAccessToken
	account.RefreshToken = newRefreshToken
	account.ExpiresAt = newExpiresAt
	h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}
	h.tokenRefreshMu.Unlock()
	if callback != nil && callback.OnReset != nil {
		callback.OnReset()
	}
	retryErr := dispatchChat(account, payload, callback)
	if retryErr != nil {
		if callback != nil && callback.HasOutput != nil && callback.HasOutput() {
			logger.Warnf("[AuthRetry] Retry after refresh produced partial output for %s; not retrying again", account.Email)
		}
		logger.Warnf("[AuthRetry] Retry after refresh failed for %s: %v", account.Email, retryErr)
		return false
	}
	logger.Infof("[AuthRetry] Token refresh + retry succeeded for %s", account.Email)
	return true
}

// transientRetryMaxAttempts is the maximum number of same-account retries
// attempted for transient upstream errors (5xx, overload, timeout) before
// rotating to a different account. Each attempt sleeps transientRetryBaseDelay
// multiplied by the attempt index (linear backoff).
const transientRetryMaxAttempts = 3

// transientRetryBaseDelay is the base delay for transient-error backoff.
// Attempt N sleeps (N * baseDelay) before retrying — so 500ms, 1s, 1.5s.
const transientRetryBaseDelay = 500 * time.Millisecond

// tryTransientRetry retries the same account after a short backoff when the
// upstream returns a transient error (5xx, overload, timeout, network blip).
// Returns true if a retry succeeded, false if all retries failed or the error
// is not transient. The account stays enabled (no rotation) on transient errs.
//
// This is called BEFORE tryRefreshAndRetry in the retry loops so transient
// upstream issues are retried in-place without churning through accounts.
func (h *Handler) tryTransientRetry(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback, err error) bool {
	// This retry policy is intentionally limited to external OpenAI-compatible
	// providers. Native Kiro/AWS and Codex have separate upstream semantics and
	// must not inherit this external-provider failover behavior.
	if !isExternalAccount(account) || err == nil || !pool.IsTransientError(err) {
		return false
	}
	// Hard quota/credit exhaustion never recovers on a same-account retry —
	// skip the backoff entirely and let the caller rotate to the next account.
	if pool.IsQuotaExhaustionError(err) {
		logger.Warnf("[TransientRetry] %s: quota/credit exhausted — skipping retry, rotating now (err: %s)",
			account.Email, truncateForLog(err.Error()))
		return false
	}
	// Endpoint-global network errors (DNS failure, connection refused) affect
	// ALL accounts sharing the same upstream endpoint. Retrying the same
	// account wastes 3×backoff before the same failure — rotate to a
	// different provider/endpoint immediately instead.
	if isEndpointGlobalError(err.Error()) {
		logger.Warnf("[TransientRetry] %s: endpoint-global network error — skipping retry, rotating to different endpoint (err: %s)",
			account.Email, truncateForLog(err.Error()))
		return false
	}
	// Per-request transient network errors (connection reset, broken pipe, EOF,
	// timeout) may succeed on retry with the SAME account — the endpoint is
	// still alive, just this particular connection dropped. Fall through to
	// the standard retry loop below.
	for attempt := 1; attempt <= transientRetryMaxAttempts; attempt++ {
		delay := time.Duration(attempt) * transientRetryBaseDelay
		logger.Warnf("[TransientRetry] %s: attempt %d/%d after %v (err: %s)",
			account.Email, attempt, transientRetryMaxAttempts, delay, truncateForLog(err.Error()))
		time.Sleep(delay)
		if callback != nil && callback.OnReset != nil {
			callback.OnReset()
		}
		retryErr := dispatchChat(account, payload, callback)
		if retryErr == nil {
			logger.Infof("[TransientRetry] %s: succeeded on attempt %d", account.Email, attempt)
			return true
		}
		if !pool.IsTransientError(retryErr) {
			// Error changed to non-transient (e.g. 401) — let the caller's
			// normal failure path handle it (refresh / rotate / disable).
			logger.Warnf("[TransientRetry] %s: error became non-transient on attempt %d: %s",
				account.Email, attempt, truncateForLog(retryErr.Error()))
			return false
		}
		if callback != nil && callback.HasOutput != nil && callback.HasOutput() {
			logger.Warnf("[TransientRetry] %s: retry produced partial output; stopping retries", account.Email)
			return false
		}
	}
	logger.Warnf("[TransientRetry] %s: exhausted %d attempts, rotating to next account",
		account.Email, transientRetryMaxAttempts)
	return false
}

// truncateForLog clips an error message to a reasonable length for log lines.
func truncateForLog(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// ensureValidToken ensures token is valid
func (h *Handler) ensureValidToken(account *config.Account) error {
	if !tokenNeedsRefresh(account, time.Now().Unix()) {
		return nil
	}

	// Antigravity accounts refresh against Google's OAuth token endpoint, not
	// the Kiro/OIDC path below. Routing them through auth.RefreshAccountToken
	// would submit a Google refresh token to an AWS endpoint and fail.
	if isAntigravityAccount(account) {
		h.tokenRefreshMu.Lock()
		defer h.tokenRefreshMu.Unlock()
		if latest := h.pool.GetByID(account.ID); latest != nil {
			account.AccessToken = latest.AccessToken
			account.RefreshToken = latest.RefreshToken
			account.ExpiresAt = latest.ExpiresAt
			if !tokenNeedsRefresh(account, time.Now().Unix()) {
				return nil
			}
		}
		if err := refreshAntigravityAccountToken(account); err != nil {
			logger.Warnf("[TokenRefresh] Antigravity refresh failed for %s: %v", account.Email, err)
			return err
		}
		h.pool.UpdateToken(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt)
		return nil
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if !tokenNeedsRefresh(account, time.Now().Unix()) {
			return nil
		}
	}

	// Check rotation map first: if a sibling already refreshed and rotated this
	// token, use the cached new tokens instead of hitting upstream.
	var accessToken, refreshToken, profileArn string
	var expiresAt int64
	var err error

	if rot := auth.CheckRotation(account.RefreshToken); rot != nil {
		logger.Infof("[TokenRefresh] %s token was rotated by sibling — using cached", account.Email)
		accessToken = rot.AccessToken
		refreshToken = rot.RefreshToken
		expiresAt = rot.ExpiresAt
		profileArn = rot.ProfileArn
		err = nil
	} else {
		accessToken, refreshToken, expiresAt, profileArn, _, _, err = auth.RefreshAccountToken(account)
	}

	if err != nil {
		logger.Warnf("[TokenRefresh] Refresh failed for %s: %v", account.Email, err)
		// Stale token retry: if the account's refresh token was already rotated
		// by a sibling account's refresh,
		// the 401 is expected — the account is actually healthy with a newer token.
		// Re-read from pool to check.
		if latest := h.pool.GetByID(account.ID); latest != nil && latest.RefreshToken != account.RefreshToken {
			logger.Infof("[TokenRefresh] %s refresh token already rotated by sibling — reusing", account.Email)
			account.AccessToken = latest.AccessToken
			account.RefreshToken = latest.RefreshToken
			account.ExpiresAt = latest.ExpiresAt
			if !tokenNeedsRefresh(account, time.Now().Unix()) {
				return nil
			}
		}
		h.handleAccountFailure(account, err, "")
		return err
	}

	if refreshToken == "" {
		refreshToken = account.RefreshToken
	}
	// Persist the rotating credential before exposing it through the pool.
	// A failed durable write must not make memory diverge from config.json.
	if err := config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt); err != nil {
		logger.Warnf("[TokenRefresh] Failed to persist refreshed token for %s: %v", account.Email, err)
		return err
	}
	account.AccessToken = accessToken
	account.RefreshToken = refreshToken
	account.ExpiresAt = expiresAt
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	if profileArn != "" {
		account.ProfileArn = profileArn
		if err := config.UpdateAccountProfileArn(account.ID, profileArn); err != nil {
			logger.Warnf("[TokenRefresh] Failed to persist profile ARN for %s: %v", account.Email, err)
		}
	}

	// Codex accounts: re-extract chatgpt_account_id from the refreshed
	// access-token JWT and persist it (it may rotate on re-issue).
	if account.AuthMethod == codexAuthMethod {
		account.AccessToken = accessToken
		refreshCodexAccountID(account)
	}

	return nil
}

// tokenNeedsRefresh uses the provider's strongest available expiry signal.
// Codex access tokens are JWTs, so their exp claim takes precedence when an
// imported account has a stale persisted ExpiresAt value.
func tokenNeedsRefresh(account *config.Account, now int64) bool {
	if isCodexAccount(account) {
		return codexTokenNeedsRefresh(account, now)
	}
	if isAntigravityAccount(account) {
		return antigravityTokenNeedsRefresh(account, now)
	}
	return account != nil && account.ExpiresAt > 0 && now >= account.ExpiresAt-tokenRefreshSkewSeconds
}

// ==================== Admin API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// verify password
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		password = r.URL.Query().Get("pwd")
	}
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	if password != config.GetPassword() {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		// Polled every ~5s by the dashboard (~76 KiB). Revalidate so unchanged
		// pool state costs a header exchange instead of a full payload.
		withETag(w, r, h.apiGetAccounts)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh must match before generic /refresh to avoid interception
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case path == "/accounts/refresh-all" && r.Method == "POST":
		h.apiRefreshAllAccounts(w, r)
	case path == "/accounts/reauth-all-banned" && r.Method == "POST":
		h.apiReauthAllBanned(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/reset-quota") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/reset-quota")
		h.apiResetAccountQuota(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/reset-credits") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/reset-credits")
		h.apiResetAccountCredits(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/reset-credits/available") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/reset-credits/available")
		h.apiGetResetCreditsAvailable(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/image-models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/image-models")
		h.apiGetAccountImageModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/probe-capabilities") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/probe-capabilities")
		h.apiProbeAccountCapabilities(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/credits") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/credits")
		h.apiRefreshAccountCredits(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/restore-refresh-token") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/restore-refresh-token")
		h.apiRestoreCodexRefreshToken(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/codex-security") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/codex-security")
		h.apiOpenCodexSecurity(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/antigravity-project") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/antigravity-project")
		h.apiAntigravityRefreshProject(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/gommo-balance") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/gommo-balance")
		h.apiRefreshGommoBalance(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh-token") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh-token")
		h.apiRefreshAccountToken(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/social/start" && r.Method == "POST":
		h.apiSocialLoginStart(w, r)
	case path == "/auth/social/poll" && r.Method == "POST":
		h.apiSocialLoginPoll(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiKiroSsoStart(w, r)
	case path == "/auth/kiro-sso/enterprise-start" && r.Method == "POST":
		h.apiKiroEnterpriseStart(w, r)
	case path == "/auth/kiro-sso/exchange" && r.Method == "POST":
		h.apiKiroSsoExchange(w, r)
	case path == "/auth/kiro-cli" && r.Method == "POST":
		h.apiImportKiroCli(w, r)
	case path == "/auth/kiro-auto-import" && r.Method == "GET":
		h.apiAutoImportKiroCli(w, r)
	case path == "/auth/kiro-import" && r.Method == "POST":
		h.apiImportKiroToken(w, r)
	case path == "/auth/kiro-api-key" && r.Method == "POST":
		h.apiImportKiroApiKey(w, r)
	case path == "/auth/external-provider" && r.Method == "POST":
		h.apiImportExternalProvider(w, r)
	case path == "/auth/codex/login" && r.Method == "POST":
		h.apiCodexLoginStart(w, r)
	case path == "/auth/codex/open-browser" && r.Method == "POST":
		h.apiCodexLoginOpenBrowser(w, r)
	case path == "/auth/codex/poll" && r.Method == "POST":
		h.apiCodexLoginPoll(w, r)
	case path == "/auth/codex/cancel" && r.Method == "POST":
		h.apiCodexLoginCancel(w, r)
	case path == "/auth/codex-import" && r.Method == "POST":
		h.apiImportCodexTokens(w, r)
	case path == "/auth/antigravity/login" && r.Method == "POST":
		h.apiAntigravityLoginStart(w, r)
	case path == "/auth/antigravity/poll" && r.Method == "POST":
		h.apiAntigravityLoginPoll(w, r)
	case path == "/auth/antigravity/cancel" && r.Method == "POST":
		h.apiAntigravityLoginCancel(w, r)
	case path == "/auth/antigravity/local" && r.Method == "GET":
		h.apiAntigravityLocalCreds(w, r)
	case path == "/auth/antigravity-import" && r.Method == "POST":
		h.apiImportAntigravityCreds(w, r)
	case path == "/auth/gommo" && r.Method == "POST":
		h.apiImportGommoProvider(w, r)
	case path == "/auth/import-9router" && r.Method == "POST":
		h.apiImportFrom9Router(w, r)
	case path == "/auth/import-9router/preview" && r.Method == "POST":
		h.apiPreview9Router(w, r)
	case path == "/auth/kiro-cli-register" && r.Method == "POST":
		h.apiRegisterKiroCli(w, r)
	case path == "/auth/sso-cache" && r.Method == "POST":
		h.apiImportSSOCache(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/capabilities" && r.Method == "GET":
		h.apiGetCapabilities(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case path == "/combos" && r.Method == "GET":
		h.apiListCombos(w, r)
	case path == "/combos" && r.Method == "POST":
		h.apiCreateCombo(w, r)
	case path == "/combo-settings" && r.Method == "GET":
		h.apiGetComboSettings(w, r)
	case path == "/combo-settings" && r.Method == "POST":
		h.apiUpdateComboSettings(w, r)
	case strings.HasPrefix(path, "/combos/") && r.Method == "GET":
		h.apiGetCombo(w, r, strings.TrimPrefix(path, "/combos/"))
	case strings.HasPrefix(path, "/combos/") && r.Method == "PUT":
		h.apiUpdateCombo(w, r, strings.TrimPrefix(path, "/combos/"))
	case strings.HasPrefix(path, "/combos/") && r.Method == "DELETE":
		h.apiDeleteCombo(w, r, strings.TrimPrefix(path, "/combos/"))
	case path == "/shutdown" && r.Method == "POST":
		h.apiShutdown(w, r)

	case strings.HasPrefix(path, "/cli-tools/apikey/") && r.Method == "GET":
		keyID := strings.TrimPrefix(path, "/cli-tools/apikey/")
		h.apiGetCliToolApiKey(w, r, keyID)

	// Model test endpoint
	case path == "/cli-tools/status" && r.Method == "GET":
		json.NewEncoder(w).Encode(getCliToolsStatus())

	case path == "/cli-tools/test-model" && r.Method == "POST":
		h.apiTestModel(w, r)

	// MITM routes (must come before generic /cli-tools/ catch-all)
	case path == "/cli-tools/mitm/status" && r.Method == "GET":
		h.apiMitmStatus(w, r)
	case path == "/cli-tools/mitm/server" && r.Method == "POST":
		h.apiMitmStart(w, r)
	case path == "/cli-tools/mitm/server" && r.Method == "DELETE":
		h.apiMitmStop(w, r)
	case path == "/cli-tools/mitm/dns" && r.Method == "PATCH":
		h.apiMitmToggleDns(w, r)
	case path == "/cli-tools/mitm/aliases" && r.Method == "PUT":
		h.apiMitmSaveAliases(w, r)
	case path == "/cli-tools/copilot-settings" && (r.Method == "GET" || r.Method == "POST" || r.Method == "DELETE"):
		h.apiCopilotSettings(w, r)

	case strings.HasPrefix(path, "/cli-tools/") && r.Method == "GET":
		toolID := strings.TrimPrefix(path, "/cli-tools/")
		h.apiGetCliToolSettings(w, r, toolID)
	case strings.HasPrefix(path, "/cli-tools/") && r.Method == "POST":
		toolID := strings.TrimPrefix(path, "/cli-tools/")
		backupFile := backupToolConfig(toolID)
		markCliToolConfigured(toolID, true)
		bw := &cliBackupWriter{ResponseWriter: w, backupFile: backupFile}
		h.apiApplyCliToolSettings(bw, r, toolID)
	case strings.HasPrefix(path, "/cli-tools/") && r.Method == "DELETE":
		toolID := strings.TrimPrefix(path, "/cli-tools/")
		backupFile := backupToolConfig(toolID)
		markCliToolConfigured(toolID, false)
		bw := &cliBackupWriter{ResponseWriter: w, backupFile: backupFile}
		h.apiResetCliToolSettings(bw, r, toolID)

	case path == "/usage/stats" && r.Method == "GET":
		// Largest polled payload (~205 KiB every 5s). Revalidation collapses
		// idle periods to a 304 header exchange.
		withETag(w, r, h.apiGetUsageStats)
	case path == "/usage/chart" && r.Method == "GET":
		h.apiGetUsageChart(w, r)
	case path == "/usage/stream" && r.Method == "GET":
		h.apiUsageStream(w, r)
	case path == "/usage/request-details" && r.Method == "GET":
		h.apiGetUsageRequestDetails(w, r)
	case path == "/usage/providers" && r.Method == "GET":
		h.apiGetUsageProviders(w, r)
	case path == "/pricing" && r.Method == "GET":
		h.apiGetPricing(w, r)
	case path == "/quota/overview" && r.Method == "GET":
		// Polled every ~5s by the quota tab (~23 KiB).
		withETag(w, r, h.apiGetQuotaOverview)
	case path == "/cache/stats" && r.Method == "GET":
		h.apiGetCacheStats(w, r)
	case path == "/compression/stats" && r.Method == "GET":
		h.apiGetCompressionStats(w, r)
	case path == "/compression/config" && r.Method == "PATCH":
		h.apiUpdateCompressionConfig(w, r)
	case path == "/pool/strategy" && r.Method == "GET":
		h.apiGetPoolStrategy(w, r)
	case path == "/pool/strategy" && r.Method == "PATCH":
		h.apiUpdatePoolStrategy(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs/stream" && r.Method == "GET":
		h.apiLogsStream(w, r)
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

// CLI tools
func (h *Handler) apiApplyCliToolSettings(w http.ResponseWriter, r *http.Request, toolID string) {
	var req struct {
		BaseURL         string            `json:"baseUrl"`
		APIKey          string            `json:"apiKey"`
		Model           string            `json:"model"`
		Models          []string          `json:"models"`
		ActiveModel     string            `json:"activeModel"`
		SubagentModel   string            `json:"subagentModel"`
		ReasoningEffort string            `json:"reasoningEffort"`
		Env             map[string]string `json:"env"`
		AgentModels     map[string]string `json:"agentModels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, 400)
		return
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, `{"error":"cannot determine home directory"}`, 500)
		return
	}
	ensureV1 := func(url string) string {
		url = strings.TrimRight(url, "/")
		if !strings.HasSuffix(url, "/v1") {
			url += "/v1"
		}
		return url
	}
	stripV1 := func(url string) string {
		url = strings.TrimRight(url, "/")
		url = strings.TrimSuffix(url, "/v1")
		return url
	}
	switch toolID {
	case "claude":
		settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		current := map[string]interface{}{}
		if data, err := os.ReadFile(settingsPath); err == nil {
			json.Unmarshal(data, &current)
		}
		env := map[string]string{}
		if existing, ok := current["env"].(map[string]interface{}); ok {
			for k, v := range existing {
				if s, ok2 := v.(string); ok2 {
					env[k] = s
				}
			}
		}
		if req.Env != nil {
			// Claude Code 2.1.220 rejects settings that define both auth
			// variables. Accept the legacy value from older callers, but persist
			// only the API-key form required by current Claude Code releases.
			if apiKey := strings.TrimSpace(req.Env["ANTHROPIC_API_KEY"]); apiKey != "" {
				env["ANTHROPIC_API_KEY"] = apiKey
			} else if legacyToken := strings.TrimSpace(req.Env["ANTHROPIC_AUTH_TOKEN"]); legacyToken != "" {
				env["ANTHROPIC_API_KEY"] = legacyToken
			}
			for k, v := range req.Env {
				if k == "ANTHROPIC_AUTH_TOKEN" || k == "ANTHROPIC_API_KEY" ||
					(strings.HasPrefix(k, "ANTHROPIC_DEFAULT_") && strings.HasSuffix(k, "_MODEL")) {
					continue
				}
				if v != "" {
					if k == "ANTHROPIC_BASE_URL" {
						v = stripV1(v)
					}
					env[k] = v
				}
			}
		}
		if env["ANTHROPIC_API_KEY"] == "" {
			env["ANTHROPIC_API_KEY"] = env["ANTHROPIC_AUTH_TOKEN"]
		}
		delete(env, "ANTHROPIC_AUTH_TOKEN")
		current["hasCompletedOnboarding"] = true
		current["env"] = env
		// Apply is an endpoint/credential operation. Model selection belongs to
		// Claude Code settings and must survive UI refreshes or older callers.
		// Replace stale UI state rather than merging it: this installation exposes
		// only model IDs backed by a working upstream route.
		current["availableModels"] = []string{
			"claude-sonnet-5",
			"claude-opus-5",
		}
		// Claude Code's automatic high-demand fallback uses its Haiku tier.
		// Route that tier to Sonnet because this installation has no Haiku route.
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = "claude-sonnet-5"
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] = "Claude Sonnet 5"
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION"] = "Claude Sonnet 5 via the local gateway"
		current["fallbackModel"] = []string{"claude-sonnet-5"}
		// `fallbackModels` was an older, non-standard key that caused the
		// advisor fallback to be confused with the primary model fallback.
		delete(current, "fallbackModels")
		data, _ := json.MarshalIndent(current, "", "  ")
		if err := os.WriteFile(settingsPath, data, 0644); err != nil {
			http.Error(w, `{"error":"failed to write config file"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "opencode":
		configPath := filepath.Join(homeDir, ".config", "opencode", "opencode.json")
		if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		current := map[string]interface{}{}
		if data, err := os.ReadFile(configPath); err == nil {
			json.Unmarshal(data, &current)
		}
		bpURL := ensureV1(req.BaseURL)
		modelsMap := make(map[string]interface{})
		modelsList := req.Models
		if len(modelsList) == 0 && req.Model != "" {
			modelsList = []string{req.Model}
		}
		for _, m := range modelsList {
			if m == "" {
				continue
			}
			modelsMap[m] = map[string]interface{}{
				"name": m,
				"modalities": map[string]interface{}{
					"input":  []string{"text", "image"},
					"output": []string{"text"},
				},
			}
		}
		activeM := req.ActiveModel
		if activeM == "" && len(modelsList) > 0 {
			activeM = modelsList[0]
		}
		subM := req.SubagentModel
		if subM == "" {
			subM = activeM
		}
		provider := map[string]interface{}{
			"npm": "@ai-sdk/openai-compatible",
			"options": map[string]string{
				"baseURL": bpURL,
				"apiKey":  req.APIKey,
			},
			"models": modelsMap,
		}
		current["provider"] = map[string]interface{}{"omniproxy": provider}
		if current["model"] != nil {
			delete(current, "model")
		}
		if activeM != "" {
			current["model"] = "omniproxy/" + activeM
		}
		if subM != "" {
			current["agent"] = map[string]interface{}{
				"explorer": map[string]interface{}{
					"description": "Fast explorer subagent for codebase exploration",
					"mode":        "subagent",
					"model":       "omniproxy/" + subM,
				},
			}
		}
		data, _ := json.MarshalIndent(current, "", "  ")
		if err := os.WriteFile(configPath, data, 0644); err != nil {
			http.Error(w, `{"error":"failed to write config file"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "cline":
		secretsDir := filepath.Join(homeDir, ".cline", "data")
		if err := os.MkdirAll(secretsDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		baseURL := stripV1(req.BaseURL)
		model := req.Model
		if model == "" {
			model = "provider/model-id"
		}
		global := map[string]interface{}{
			"actModeApiProvider":    "openai",
			"planModeApiProvider":   "openai",
			"openAiBaseUrl":         baseURL,
			"openAiModelId":         model,
			"planModeOpenAiModelId": model,
		}
		globalData, _ := json.MarshalIndent(global, "", "  ")
		if err := os.WriteFile(filepath.Join(secretsDir, "globalState.json"), globalData, 0644); err != nil {
			http.Error(w, `{"error":"failed to write globalState.json"}`, 500)
			return
		}
		secrets := map[string]string{"openAiApiKey": req.APIKey}
		secretsData, _ := json.MarshalIndent(secrets, "", "  ")
		if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), secretsData, 0644); err != nil {
			http.Error(w, `{"error":"failed to write secrets.json"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "codex":
		codexDir := filepath.Join(homeDir, ".codex")
		if err := os.MkdirAll(codexDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		model := req.Model
		if model == "" {
			model = "provider/model-id"
		}
		subagent := req.SubagentModel
		if subagent == "" {
			subagent = model
		}
		bpURL := ensureV1(req.BaseURL)
		effort := strings.TrimSpace(req.ReasoningEffort)
		if effort != "low" && effort != "medium" && effort != "high" {
			effort = "medium"
		}
		if err := MergeCodexConfig(homeDir, model, bpURL, subagent, effort); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"failed to merge config: %v"}`, err), 500)
			return
		}
		auth := map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": req.APIKey}
		authData, _ := json.MarshalIndent(auth, "", "  ")
		if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), authData, 0644); err != nil {
			http.Error(w, `{"error":"failed to write auth.json"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "kilocode":
		kiloDir := filepath.Join(homeDir, ".local", "share", "kilo")
		if err := os.MkdirAll(kiloDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		bpURL := ensureV1(req.BaseURL)
		model := req.Model
		if model == "" {
			model = "provider/model-id"
		}
		auth := map[string]interface{}{
			"openai-compatible": map[string]interface{}{
				"type":    "api-key",
				"apiKey":  req.APIKey,
				"baseUrl": bpURL,
				"model":   model,
			},
		}
		data, _ := json.MarshalIndent(auth, "", "  ")
		if err := os.WriteFile(filepath.Join(kiloDir, "auth.json"), data, 0644); err != nil {
			http.Error(w, `{"error":"failed to write auth.json"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "deepseek":
		deepseekDir := filepath.Join(homeDir, ".deepseek")
		if err := os.MkdirAll(deepseekDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		bpURL := ensureV1(req.BaseURL)
		model := req.Model
		if model == "" {
			model = "provider/model-id"
		}
		tomlContent := fmt.Sprintf(`provider = "openai"

[providers.openai]
base_url = "%s"
api_key = "%s"
model = "%s"
`, bpURL, req.APIKey, model)
		if err := os.WriteFile(filepath.Join(deepseekDir, "config.toml"), []byte(tomlContent), 0644); err != nil {
			http.Error(w, `{"error":"failed to write config.toml"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "jcode":
		jcodeDir := filepath.Join(homeDir, ".jcode")
		if err := os.MkdirAll(jcodeDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		jcodeBPURL := ensureV1(req.BaseURL)
		jcodeModelsList := req.Models
		if len(jcodeModelsList) == 0 && req.Model != "" {
			jcodeModelsList = []string{req.Model}
		}
		if len(jcodeModelsList) == 0 {
			jcodeModelsList = []string{"provider/model-id"}
		}
		jcodeDefaultModel := jcodeModelsList[0]
		jcodeModelsToml := ""
		for _, m := range jcodeModelsList {
			jcodeModelsToml += fmt.Sprintf(`[[providers.9router.models]]
id = "%s"
`, m)
		}
		jcodeTOML := fmt.Sprintf(`[providers.9router]
type = "openai-compatible"
base_url = "%s"
auth = "bearer"
api_key_env = "JCODE_9ROUTER_API_KEY"
env_file = "provider-9router.env"
default_model = "%s"
requires_api_key = true
%s`, jcodeBPURL, jcodeDefaultModel, jcodeModelsToml)
		if err := os.WriteFile(filepath.Join(jcodeDir, "config.toml"), []byte(jcodeTOML), 0644); err != nil {
			http.Error(w, `{"error":"failed to write config.toml"}`, 500)
			return
		}
		envDir := filepath.Join(homeDir, ".config", "jcode")
		os.MkdirAll(envDir, 0755)
		envContent := fmt.Sprintf("# jcode provider environment variables\nJCODE_9ROUTER_API_KEY=\"%s\"\n", req.APIKey)
		os.WriteFile(filepath.Join(envDir, "provider-9router.env"), []byte(envContent), 0644)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "hermes":
		hermesDir := filepath.Join(homeDir, ".hermes")
		if err := os.MkdirAll(hermesDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		hermesBPURL := ensureV1(req.BaseURL)
		hermesModel := req.Model
		if hermesModel == "" {
			hermesModel = defaultAgentModel
		}
		hermesModel = hermesModelID(hermesModel)
		yamlContent := ""
		configPath := filepath.Join(hermesDir, "config.yaml")
		if data, err := os.ReadFile(configPath); err == nil {
			yamlContent = string(data)
		}
		catalog := h.omniProxyModelCatalog(hermesModel, imageAgentModel, deepResearchModel)
		providerBlock := hermesProviderBlock(hermesBPURL, req.APIKey, catalog)
		if strings.TrimSpace(yamlContent) == "" {
			yamlContent = fmt.Sprintf(`model:
  default: "%s"
  provider: "omniproxy"
  base_url: "%s"
`, hermesModel, hermesBPURL)
		}
		// Always update the top-level model: section so default, provider,
		// base_url, api_key, and api_mode point to OmniProxy — even when the
		// config already exists with a different provider (e.g. omniroute).
		contextWindow, maxTokens, hasLimits := h.modelTokenLimits(hermesModel)
		if hasLimits {
			yamlContent = upsertYAMLModelSection(yamlContent, hermesModel, "omniproxy", hermesBPURL, req.APIKey, contextWindow, maxTokens)
		} else {
			yamlContent = upsertYAMLModelSection(yamlContent, hermesModel, "omniproxy", hermesBPURL, req.APIKey)
		}
		yamlContent = upsertYAMLProviderBlock(yamlContent, "omniproxy", providerBlock)
		yamlContent = upsertYAMLAuxiliaryModels(yamlContent, hermesBPURL, req.APIKey)
		if err := os.WriteFile(filepath.Join(hermesDir, "config.yaml"), []byte(yamlContent), 0644); err != nil {
			http.Error(w, `{"error":"failed to write config.yaml"}`, 500)
			return
		}
		envContent2 := fmt.Sprintf("OPENAI_API_KEY=%s\n", req.APIKey)
		os.WriteFile(filepath.Join(hermesDir, ".env"), []byte(envContent2), 0644)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "droid":
		droidDir := filepath.Join(homeDir, ".factory")
		if err := os.MkdirAll(droidDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		droidBPURL := ensureV1(req.BaseURL)
		droidModelsList := req.Models
		if len(droidModelsList) == 0 && req.Model != "" {
			droidModelsList = []string{req.Model}
		}
		if len(droidModelsList) == 0 {
			droidModelsList = []string{"provider/model-id"}
		}
		droidActiveM := req.ActiveModel
		if droidActiveM == "" {
			droidActiveM = droidModelsList[0]
		}
		currentDroid := map[string]interface{}{}
		if data, err := os.ReadFile(filepath.Join(droidDir, "settings.json")); err == nil {
			json.Unmarshal(data, &currentDroid)
		}
		var customModels []interface{}
		if existing, ok := currentDroid["customModels"].([]interface{}); ok {
			for _, cm := range existing {
				if cmMap, ok2 := cm.(map[string]interface{}); ok2 {
					id, _ := cmMap["id"].(string)
					if strings.HasPrefix(id, "custom:9Router") {
						continue
					}
					customModels = append(customModels, cm)
				}
			}
		}
		droidIdx := 0
		for i, m := range droidModelsList {
			if m == "" {
				continue
			}
			entry := map[string]interface{}{
				"model":           m,
				"id":              fmt.Sprintf("custom:9Router-%d", i),
				"index":           i,
				"baseUrl":         droidBPURL,
				"apiKey":          req.APIKey,
				"displayName":     m,
				"maxOutputTokens": 131072,
				"noImageSupport":  false,
				"provider":        "openai",
			}
			if m == droidActiveM {
				entry["index"] = droidIdx
				droidIdx++
				customModels = append([]interface{}{entry}, customModels...)
			} else {
				entry["index"] = len(customModels) + droidIdx
				droidIdx++
				customModels = append(customModels, entry)
			}
		}
		currentDroid["customModels"] = customModels
		droidData, _ := json.MarshalIndent(currentDroid, "", "  ")
		if err := os.WriteFile(filepath.Join(droidDir, "settings.json"), droidData, 0644); err != nil {
			http.Error(w, `{"error":"failed to write settings.json"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "openclaw":
		ocDir := filepath.Join(homeDir, ".openclaw")
		if err := os.MkdirAll(ocDir, 0755); err != nil {
			http.Error(w, `{"error":"cannot create config directory"}`, 500)
			return
		}
		ocBPURL := ensureV1(req.BaseURL)
		ocModel := req.Model
		if ocModel == "" {
			ocModel = defaultAgentModel
		}
		currentOC := map[string]interface{}{}
		if data, err := os.ReadFile(filepath.Join(ocDir, "openclaw.json")); err == nil {
			json.Unmarshal(data, &currentOC)
		}
		modelsSec, ok := currentOC["models"].(map[string]interface{})
		if !ok {
			modelsSec = map[string]interface{}{}
		}
		providers, ok := modelsSec["providers"].(map[string]interface{})
		if !ok {
			providers = map[string]interface{}{}
		}
		omniProvider := map[string]interface{}{
			"baseUrl": ocBPURL,
			"apiKey":  req.APIKey,
			"api":     "openai-completions",
		}
		if existing, ok := providers["omniproxy"].(map[string]interface{}); ok {
			for key, value := range existing {
				omniProvider[key] = value
			}
			omniProvider["baseUrl"] = ocBPURL
			omniProvider["apiKey"] = req.APIKey
			omniProvider["api"] = "openai-completions"
		}
		catalogExtras := []string{ocModel, imageAgentModel, deepResearchModel}
		for _, agentModel := range req.AgentModels {
			catalogExtras = append(catalogExtras, agentModel)
		}
		catalog := h.omniProxyModelCatalog(catalogExtras...)
		mergeOpenClawProviderCatalog(omniProvider, catalog)
		providers["omniproxy"] = omniProvider
		modelsSec["providers"] = providers
		currentOC["models"] = modelsSec
		agentModels := map[string]string{}
		if req.AgentModels != nil {
			agentModels = req.AgentModels
		}
		agentConfig, nested := currentOC["agents"].(map[string]interface{})
		if !nested {
			agentConfig = currentOC
		}
		if _, hasList := agentConfig["list"]; !hasList {
			agentConfig["list"] = []interface{}{map[string]interface{}{
				"id": "default", "model": openClawModelRef(ocModel), "primary": true,
			}}
		}
		mergeOpenClawAgents(agentConfig, ocModel, agentModels)
		defaults, _ := agentConfig["defaults"].(map[string]interface{})
		if defaults == nil {
			defaults = map[string]interface{}{}
			agentConfig["defaults"] = defaults
		}
		allowed, _ := defaults["models"].(map[string]interface{})
		if allowed == nil {
			allowed = map[string]interface{}{}
			defaults["models"] = allowed
		}
		mergeOpenClawAllowedCatalog(allowed, catalog)
		if nested {
			currentOC["agents"] = agentConfig
		}
		ocData, _ := json.MarshalIndent(currentOC, "", "  ")
		if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), ocData, 0644); err != nil {
			http.Error(w, `{"error":"failed to write openclaw.json"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

		setCliToolSettings(toolID, &CliToolSettings{
			BaseURL:       req.BaseURL,
			APIKey:        req.APIKey,
			Model:         req.Model,
			Models:        req.Models,
			ActiveModel:   req.ActiveModel,
			SubagentModel: req.SubagentModel,
			Env:           req.Env,
			AgentModels:   req.AgentModels,
		})
	default:
		http.Error(w, `{"error":"unknown tool"}`, 404)
	}
}

func (h *Handler) apiResetCliToolSettings(w http.ResponseWriter, r *http.Request, toolID string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, `{"error":"cannot determine home directory"}`, 500)
		return
	}
	switch toolID {
	case "claude":
		os.Remove(filepath.Join(homeDir, ".claude", "settings.json"))
	case "opencode":
		os.Remove(filepath.Join(homeDir, ".config", "opencode", "opencode.json"))
	case "cline":
		os.Remove(filepath.Join(homeDir, ".cline", "data", "globalState.json"))
		os.Remove(filepath.Join(homeDir, ".cline", "data", "secrets.json"))
	case "codex":
		os.Remove(filepath.Join(homeDir, ".codex", "config.toml"))
		os.Remove(filepath.Join(homeDir, ".codex", "auth.json"))
	case "kilocode":
		os.Remove(filepath.Join(homeDir, ".local", "share", "kilo", "auth.json"))
	case "deepseek":
		os.Remove(filepath.Join(homeDir, ".deepseek", "config.toml"))
	case "jcode":
		os.Remove(filepath.Join(homeDir, ".jcode", "config.toml"))
		os.Remove(filepath.Join(homeDir, ".config", "jcode", "provider-9router.env"))
	case "hermes":
		os.Remove(filepath.Join(homeDir, ".hermes", "config.yaml"))
		os.Remove(filepath.Join(homeDir, ".hermes", ".env"))
	case "droid":
		droidPath := filepath.Join(homeDir, ".factory", "settings.json")
		if data, err := os.ReadFile(droidPath); err == nil {
			var cfg map[string]interface{}
			if json.Unmarshal(data, &cfg) == nil {
				if cms, ok := cfg["customModels"].([]interface{}); ok {
					var kept []interface{}
					for _, cm := range cms {
						if cmMap, ok2 := cm.(map[string]interface{}); ok2 {
							id, _ := cmMap["id"].(string)
							if strings.HasPrefix(id, "custom:9Router") {
								continue
							}
							kept = append(kept, cm)
						}
					}
					if len(kept) == 0 {
						delete(cfg, "customModels")
					} else {
						cfg["customModels"] = kept
					}
					out, _ := json.MarshalIndent(cfg, "", "  ")
					os.WriteFile(droidPath, out, 0644)
				}
			}
		}
	case "openclaw":
		ocPath := filepath.Join(homeDir, ".openclaw", "openclaw.json")
		if data, err := os.ReadFile(ocPath); err == nil {
			var cfg map[string]interface{}
			if json.Unmarshal(data, &cfg) == nil {
				delete(cfg, "models")
				delete(cfg, "agents")
				out, _ := json.MarshalIndent(cfg, "", "  ")
				os.WriteFile(ocPath, out, 0644)
			}
		}
	default:
		http.Error(w, `{"error":"unknown tool"}`, 404)
		return
	}
	delCliToolSettings(toolID)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type tomlSec struct {
	name string
	kv   map[string]string
}

func parseTOML(data []byte) []tomlSec {
	var secs []tomlSec
	var cur *tomlSec
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' && line[len(line)-1] == ']' {
			if cur != nil {
				secs = append(secs, *cur)
			}
			cur = &tomlSec{name: line[1 : len(line)-1], kv: make(map[string]string)}
			continue
		}
		if cur == nil {
			cur = &tomlSec{name: "", kv: make(map[string]string)}
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.Trim(strings.TrimSpace(parts[1]), "\"")
			cur.kv[k] = v
		}
	}
	if cur != nil {
		secs = append(secs, *cur)
	}
	return secs
}

func readCliToolSettingsFromFile(toolID string) *CliToolSettings {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	switch toolID {
	case "codex":
		path := filepath.Join(homeDir, ".codex", "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		secs := parseTOML(data)
		var model, modelProvider, baseUrl, subagentModel string
		for _, s := range secs {
			if s.name == "" {
				if v, ok := s.kv["model"]; ok {
					model = v
				}
				if v, ok := s.kv["model_provider"]; ok {
					modelProvider = v
				}
			}
		}
		// Read base_url from the active provider section
		if modelProvider != "" {
			provSection := "model_providers." + modelProvider
			for _, s := range secs {
				if s.name == provSection {
					if v, ok := s.kv["base_url"]; ok {
						baseUrl = v
					}
					break
				}
			}
		}
		// Read subagent model
		for _, s := range secs {
			if s.name == "agents.subagent" {
				if v, ok := s.kv["model"]; ok {
					subagentModel = v
				}
				break
			}
		}
		return &CliToolSettings{
			BaseURL:       baseUrl,
			Model:         model,
			ActiveModel:   model,
			SubagentModel: subagentModel,
			Config:        raw,
		}

	case "opencode":
		path := filepath.Join(homeDir, ".config", "opencode", "opencode.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return nil
		}
		activeModel, _ := cfg["model"].(string)
		providers, _ := cfg["provider"].(map[string]interface{})
		var baseUrl, apiKey string
		var models []string
		var subagentModel string
		if providers != nil {
			// Check both new "omniproxy" and legacy "superkiro" provider keys.
			p, _ := providers["omniproxy"].(map[string]interface{})
			if p == nil {
				p, _ = providers["superkiro"].(map[string]interface{})
			}
			if p != nil {
				if opts, _ := p["options"].(map[string]interface{}); opts != nil {
					baseUrl, _ = opts["baseURL"].(string)
					apiKey, _ = opts["apiKey"].(string)
				}
				if modelsMap, _ := p["models"].(map[string]interface{}); modelsMap != nil {
					for name := range modelsMap {
						models = append(models, name)
					}
				}
			}
		}
		if agent, _ := cfg["agent"].(map[string]interface{}); agent != nil {
			if explorer, _ := agent["explorer"].(map[string]interface{}); explorer != nil {
				if m, _ := explorer["model"].(string); m != "" {
					subagentModel = strings.TrimPrefix(m, "omniproxy/")
				}
			}
		}
		return &CliToolSettings{
			BaseURL:       baseUrl,
			APIKey:        apiKey,
			Models:        models,
			ActiveModel:   strings.TrimPrefix(activeModel, "omniproxy/"),
			SubagentModel: subagentModel,
			Config:        raw,
		}

	case "claude":
		path := filepath.Join(homeDir, ".claude", "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return nil
		}
		env, _ := cfg["env"].(map[string]interface{})
		if env == nil {
			return nil
		}
		baseUrl, _ := env["ANTHROPIC_BASE_URL"].(string)
		apiKey, _ := env["ANTHROPIC_API_KEY"].(string)
		if apiKey == "" {
			// Read existing configurations written by older OmniProxy versions.
			apiKey, _ = env["ANTHROPIC_AUTH_TOKEN"].(string)
		}
		envMap := make(map[string]string)
		for k, v := range env {
			if s, ok := v.(string); ok {
				envMap[k] = s
			}
		}
		return &CliToolSettings{
			BaseURL: baseUrl,
			APIKey:  apiKey,
			Env:     envMap,
			Config:  raw,
		}

	case "cline":
		path := filepath.Join(homeDir, ".cline", "data", "globalState.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return nil
		}
		model, _ := cfg["openAiModelId"].(string)
		baseUrl, _ := cfg["openAiBaseUrl"].(string)
		apiKey := ""
		if secretsData, err := os.ReadFile(filepath.Join(homeDir, ".cline", "data", "secrets.json")); err == nil {
			var secrets map[string]string
			if json.Unmarshal(secretsData, &secrets) == nil {
				apiKey = secrets["openAiApiKey"]
			}
		}
		return &CliToolSettings{
			BaseURL: baseUrl,
			APIKey:  apiKey,
			Model:   model,
			Config:  raw,
		}

	case "deepseek":
		path := filepath.Join(homeDir, ".deepseek", "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		secs := parseTOML(data)
		var baseUrl, apiKey, model string
		for _, s := range secs {
			if s.name == "providers.openai" || s.name == "providers.openai-compatible" {
				if v, ok := s.kv["base_url"]; ok {
					baseUrl = v
				}
				if v, ok := s.kv["api_key"]; ok {
					apiKey = v
				}
				if v, ok := s.kv["model"]; ok {
					model = v
				}
			}
		}
		return &CliToolSettings{
			BaseURL: baseUrl,
			APIKey:  apiKey,
			Model:   model,
			Config:  raw,
		}

	case "jcode":
		path := filepath.Join(homeDir, ".jcode", "config.toml")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		secs := parseTOML(data)
		var baseUrl, apiKey, defaultModel string
		var models []string
		for _, s := range secs {
			if s.name == "providers.9router" {
				if v, ok := s.kv["base_url"]; ok {
					baseUrl = v
				}
				if v, ok := s.kv["default_model"]; ok {
					defaultModel = v
				}
			}
			if strings.HasPrefix(s.name, "providers.9router.models") {
				if v, ok := s.kv["id"]; ok {
					models = append(models, v)
				}
			}
		}
		envData, err := os.ReadFile(filepath.Join(homeDir, ".config", "jcode", "provider-9router.env"))
		if err == nil {
			for _, line := range strings.Split(string(envData), "\n") {
				if strings.HasPrefix(line, "JCODE_9ROUTER_API_KEY=") {
					apiKey = strings.Trim(strings.TrimPrefix(line, "JCODE_9ROUTER_API_KEY="), "\"")
				}
			}
		}
		return &CliToolSettings{
			BaseURL: baseUrl,
			APIKey:  apiKey,
			Model:   defaultModel,
			Models:  models,
			Config:  raw,
		}

	case "kilo":
		path := filepath.Join(homeDir, ".local", "share", "kilo", "auth.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return nil
		}
		entry, _ := cfg["openai-compatible"].(map[string]interface{})
		var baseUrl, apiKey, model string
		if entry != nil {
			baseUrl, _ = entry["baseUrl"].(string)
			apiKey, _ = entry["apiKey"].(string)
			model, _ = entry["model"].(string)
		}
		return &CliToolSettings{
			BaseURL: baseUrl,
			APIKey:  apiKey,
			Model:   model,
			Config:  raw,
		}

	case "hermes":
		yamlPath := filepath.Join(homeDir, ".hermes", "config.yaml")
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			return nil
		}
		raw := string(data)
		var baseUrl, model string
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "base_url:") {
				baseUrl = strings.TrimSpace(strings.TrimPrefix(line, "base_url:"))
				baseUrl = strings.Trim(baseUrl, "\"")
			}
			if strings.HasPrefix(line, "default:") {
				model = strings.TrimSpace(strings.TrimPrefix(line, "default:"))
				model = strings.Trim(model, "\"")
			}
		}
		apiKey := ""
		if envData, err := os.ReadFile(filepath.Join(homeDir, ".hermes", ".env")); err == nil {
			for _, line := range strings.Split(string(envData), "\n") {
				if strings.HasPrefix(line, "OPENAI_API_KEY=") {
					apiKey = strings.Trim(strings.TrimPrefix(line, "OPENAI_API_KEY="), "\"")
					break
				}
			}
		}
		return &CliToolSettings{
			BaseURL: baseUrl,
			APIKey:  apiKey,
			Model:   model,
			Config:  raw,
		}

	case "droid":
		path := filepath.Join(homeDir, ".factory", "settings.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return nil
		}
		models, _ := cfg["customModels"].([]interface{})
		var ourModels []string
		var activeModel string
		var baseUrl, apiKey string
		if models != nil {
			for _, m := range models {
				mm, _ := m.(map[string]interface{})
				if mm == nil {
					continue
				}
				id, _ := mm["id"].(string)
				if !strings.HasPrefix(id, "custom:9Router") {
					continue
				}
				if mName, _ := mm["model"].(string); mName != "" {
					ourModels = append(ourModels, mName)
				}
				idx, _ := mm["index"].(float64)
				if activeModel == "" || (idx == 0) {
					if mName, _ := mm["model"].(string); mName != "" {
						activeModel = mName
					}
				}
				if b, _ := mm["baseUrl"].(string); b != "" {
					baseUrl = b
				}
				if k, _ := mm["apiKey"].(string); k != "" {
					apiKey = k
				}
			}
		}
		return &CliToolSettings{
			BaseURL:     baseUrl,
			APIKey:      apiKey,
			Models:      ourModels,
			ActiveModel: activeModel,
			Config:      raw,
		}

	case "openclaw":
		path := filepath.Join(homeDir, ".openclaw", "openclaw.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) != nil {
			return nil
		}
		modelsSec, _ := cfg["models"].(map[string]interface{})
		var baseUrl, apiKey, model string
		var agentModels map[string]string
		if modelsSec != nil {
			if providers, _ := modelsSec["providers"].(map[string]interface{}); providers != nil {
				// Check both new "omniproxy" and legacy "superkiro" keys.
				p, _ := providers["omniproxy"].(map[string]interface{})
				if p == nil {
					p, _ = providers["superkiro"].(map[string]interface{})
				}
				if p != nil {
					baseUrl, _ = p["baseUrl"].(string)
					apiKey, _ = p["apiKey"].(string)
					if ms, _ := p["models"].([]interface{}); len(ms) > 0 {
						if m, _ := ms[0].(map[string]interface{}); m != nil {
							model, _ = m["id"].(string)
						}
					}
				}
				if model == "" {
					if p, _ := providers["9router"].(map[string]interface{}); p != nil {
						baseUrl, _ = p["baseUrl"].(string)
						apiKey, _ = p["apiKey"].(string)
						if ms, _ := p["models"].([]interface{}); len(ms) > 0 {
							if m, _ := ms[0].(map[string]interface{}); m != nil {
								model, _ = m["id"].(string)
							}
						}
					}
				}
			}
		}
		agentModels = map[string]string{}
		if agents, _ := cfg["agents"].(map[string]interface{}); agents != nil {
			if list, _ := agents["list"].([]interface{}); list != nil {
				for _, a := range list {
					if am, _ := a.(map[string]interface{}); am != nil {
						if id, _ := am["id"].(string); id != "" {
							if m, _ := am["model"].(string); m != "" {
								m = strings.TrimPrefix(m, "omniproxy/")
								agentModels[id] = strings.TrimPrefix(m, "9router/")
							}
						}
					}
				}
			}
		}
		return &CliToolSettings{
			BaseURL:     baseUrl,
			APIKey:      apiKey,
			Model:       model,
			AgentModels: agentModels,
			Config:      raw,
		}

	case "copilot":
		path := filepath.Join(homeDir, ".config", "Code", "User", "chatLanguageModels.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		raw := string(data)
		var entries []map[string]interface{}
		if json.Unmarshal(data, &entries) != nil {
			return nil
		}
		for _, e := range entries {
			title, _ := e["title"].(string)
			if !strings.EqualFold(title, "OmniProxy") && !strings.EqualFold(title, "SuperKiro") {
				continue
			}
			baseUrl, _ := e["baseUrl"].(string)
			apiKey, _ := e["apiKey"].(string)
			model, _ := e["model"].(string)
			return &CliToolSettings{
				BaseURL: baseUrl,
				APIKey:  apiKey,
				Models:  []string{model},
				Config:  raw,
			}
		}
		return nil
	}

	return nil
}

func (h *Handler) apiGetCliToolSettings(w http.ResponseWriter, r *http.Request, toolID string) {
	s := getCliToolSettings(toolID)
	if s == nil {
		s = readCliToolSettingsFromFile(toolID)
	}
	if s == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no settings found"})
		return
	}
	json.NewEncoder(w).Encode(s)
}

func (h *Handler) apiGetCliToolApiKey(w http.ResponseWriter, r *http.Request, keyID string) {
	entry := config.GetApiKeyEntry(keyID)
	if entry == nil {
		http.Error(w, `{"error":"API key not found"}`, 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"key": entry.Key})
}

// ---- Model Test ----
func (h *Handler) apiTestModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "Invalid JSON"})
		return
	}
	if req.Model == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "model is required"})
		return
	}

	port := config.GetPort()
	host := config.GetHost()
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d/v1/chat/completions", host, port)

	payload := map[string]interface{}{
		"model":      req.Model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	}
	body, _ := json.Marshal(payload)

	httpReq, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	httpReq.Header.Set("Content-Type", "application/json")

	if config.IsApiKeyRequired() {
		var apiKey string
		if config.HasApiKeys() {
			for _, entry := range config.ListApiKeys() {
				if entry.Enabled {
					apiKey = entry.Key
					break
				}
			}
		}
		if apiKey == "" {
			apiKey = config.GetApiKey()
		}
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start).Milliseconds()
	latencyMs := latency
	if latencyMs < 0 {
		latencyMs = 0
	}

	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": false, "latencyMs": latencyMs, "error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true, "latencyMs": latencyMs,
		})
	} else {
		errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		var errResp struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&errResp) == nil && errResp.Error != "" {
			errMsg = errResp.Error
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": false, "latencyMs": latencyMs, "error": errMsg,
		})
	}
}

// ---- MITM Handlers ----
var (
	mitmRunning bool
	mitmMu      sync.RWMutex
)

type mitmStatusResp struct {
	Running bool            `json:"running"`
	Cert    bool            `json:"cert"`
	DNS     map[string]bool `json:"dns"`
}

func (h *Handler) apiMitmStatus(w http.ResponseWriter, r *http.Request) {
	mitmMu.RLock()
	running := mitmRunning
	mitmMu.RUnlock()

	// Check DNS status for each tool
	dnsStatus := map[string]bool{
		"antigravity": false,
		"copilot":     false,
		"kiro":        false,
	}
	hostsData, err := os.ReadFile(hostsFilePath)
	if err == nil {
		hostsStr := string(hostsData)
		if strings.Contains(hostsStr, "# 9router antigravity") {
			dnsStatus["antigravity"] = true
		}
		if strings.Contains(hostsStr, "# 9router copilot") {
			dnsStatus["copilot"] = true
		}
		if strings.Contains(hostsStr, "# 9router kiro") {
			dnsStatus["kiro"] = true
		}
	}

	json.NewEncoder(w).Encode(mitmStatusResp{
		Running: running,
		Cert:    false,
		DNS:     dnsStatus,
	})
}

func (h *Handler) apiMitmStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey   string `json:"apiKey"`
		SudoPass string `json:"sudoPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, 400)
		return
	}
	if req.APIKey == "" {
		http.Error(w, `{"error":"API key required"}`, 400)
		return
	}

	mitmMu.Lock()
	mitmRunning = true
	mitmMu.Unlock()

	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "MITM server started"})
}

func (h *Handler) apiMitmStop(w http.ResponseWriter, r *http.Request) {
	mitmMu.Lock()
	mitmRunning = false
	mitmMu.Unlock()

	// Remove all DNS entries
	removeMitmDnsEntries()

	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "MITM server stopped"})
}

func (h *Handler) apiMitmToggleDns(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool     string `json:"tool"`
		Action   string `json:"action"` // "enable" or "disable"
		SudoPass string `json:"sudoPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, 400)
		return
	}
	validTools := map[string]bool{"antigravity": true, "copilot": true, "kiro": true}
	if !validTools[req.Tool] {
		http.Error(w, `{"error":"unknown tool"}`, 400)
		return
	}

	if req.Action == "enable" {
		addMitmDnsEntry(req.Tool)
	} else if req.Action == "disable" {
		removeMitmDnsEntry(req.Tool)
	} else {
		http.Error(w, `{"error":"action must be enable or disable"}`, 400)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) apiMitmSaveAliases(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool     string            `json:"tool"`
		Mappings map[string]string `json:"mappings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, 400)
		return
	}
	homeDir, _ := os.UserHomeDir()
	mitmDir := filepath.Join(homeDir, ".omniproxy", "mitm")
	os.MkdirAll(mitmDir, 0755)
	aliasesPath := filepath.Join(mitmDir, "aliases.json")

	aliases := map[string]map[string]string{}
	if data, err := os.ReadFile(aliasesPath); err == nil {
		json.Unmarshal(data, &aliases)
	}
	aliases[req.Tool] = req.Mappings
	data, _ := json.MarshalIndent(aliases, "", "  ")
	os.WriteFile(aliasesPath, data, 0644)

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// DNS helpers
var mitmToolHosts = map[string][]string{
	"antigravity": {"daily-cloudcode-pa.googleapis.com", "cloudcode-pa.googleapis.com"},
	"copilot":     {"api.individual.githubcopilot.com"},
	"kiro":        {"runtime.us-east-1.kiro.dev", "q.us-east-1.amazonaws.com", "codewhisperer.us-east-1.amazonaws.com"},
}

func addMitmDnsEntry(tool string) {
	hosts, ok := mitmToolHosts[tool]
	if !ok {
		return
	}
	marker := "# 9router " + tool
	entries := ""
	for _, h := range hosts {
		entries += "127.0.0.1 " + h + " " + marker + "\n"
	}

	data, err := os.ReadFile(hostsFilePath)
	if err != nil {
		return
	}
	content := string(data)

	// Remove old entries for this tool
	lines := strings.Split(content, "\n")
	var newLines []string
	for _, line := range lines {
		if strings.Contains(line, marker) {
			continue
		}
		newLines = append(newLines, line)
	}
	newContent := strings.Join(newLines, "\n") + "\n" + entries
	os.WriteFile(hostsTmpPath, []byte(newContent), 0644)
	// Try to copy with sudo, fall back to direct write
	if err := atomicRename(hostsTmpPath, hostsFilePath); err != nil {
		os.WriteFile(hostsFilePath, []byte(newContent), 0644)
	}
}

func removeMitmDnsEntry(tool string) {
	marker := "# 9router " + tool
	data, err := os.ReadFile(hostsFilePath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	var newLines []string
	for _, line := range lines {
		if strings.Contains(line, marker) {
			continue
		}
		newLines = append(newLines, line)
	}
	newContent := strings.Join(newLines, "\n")
	os.WriteFile(hostsTmpPath, []byte(newContent), 0644)
	atomicRename(hostsTmpPath, hostsFilePath)
}

func removeMitmDnsEntries() {
	for tool := range mitmToolHosts {
		removeMitmDnsEntry(tool)
	}
}

// ---- Copilot settings backend ----
func (h *Handler) apiCopilotSettings(w http.ResponseWriter, r *http.Request) {
	homeDir, _ := os.UserHomeDir()
	copilotDir := filepath.Join(homeDir, ".config", "Code", "User")
	modelsPath := filepath.Join(copilotDir, "chatLanguageModels.json")

	switch r.Method {
	case "GET":
		var cfg struct {
			Installed  bool                     `json:"installed"`
			Models     []map[string]interface{} `json:"models"`
			Has9Router bool                     `json:"has9Router"`
		}
		if data, err := os.ReadFile(modelsPath); err == nil {
			var models []map[string]interface{}
			if json.Unmarshal(data, &models) == nil {
				cfg.Models = models
				for _, m := range models {
					if title, _ := m["title"].(string); strings.Contains(strings.ToLower(title), "superkiro") || strings.Contains(strings.ToLower(title), "omniproxy") {
						cfg.Has9Router = true
						break
					}
				}
			}
		}
		cfg.Installed = true
		json.NewEncoder(w).Encode(cfg)

	case "POST":
		backupFile := backupToolConfig("copilot")
		var req struct {
			BaseURL string   `json:"baseUrl"`
			APIKey  string   `json:"apiKey"`
			Models  []string `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, 400)
			return
		}
		if backupFile != "" {
			w.Header().Set("X-Cli-Backup", backupFile)
		}
		os.MkdirAll(copilotDir, 0755)

		var existing []map[string]interface{}
		if data, err := os.ReadFile(modelsPath); err == nil {
			json.Unmarshal(data, &existing)
		}

		// Remove old superkiro entries
		var kept []map[string]interface{}
		for _, m := range existing {
			title, _ := m["title"].(string)
			if strings.Contains(strings.ToLower(title), "superkiro") || strings.Contains(strings.ToLower(title), "omniproxy") {
				continue
			}
			kept = append(kept, m)
		}

		modelsList := req.Models
		if len(modelsList) == 0 {
			modelsList = []string{"provider/model-id"}
		}
		for _, m := range modelsList {
			kept = append(kept, map[string]interface{}{
				"title":    "OmniProxy",
				"provider": "openai",
				"model":    m,
				"apiKey":   req.APIKey,
				"baseUrl":  req.BaseURL,
			})
		}

		data, _ := json.MarshalIndent(kept, "", "  ")
		if err := os.WriteFile(modelsPath, data, 0644); err != nil {
			http.Error(w, `{"error":"failed to write settings"}`, 500)
			return
		}
		markCliToolConfigured("copilot", true)
		setCliToolSettings("copilot", &CliToolSettings{
			BaseURL: req.BaseURL,
			APIKey:  req.APIKey,
			Models:  req.Models,
		})
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case "DELETE":
		backupFile := backupToolConfig("copilot")
		if backupFile != "" {
			w.Header().Set("X-Cli-Backup", backupFile)
		}
		markCliToolConfigured("copilot", false)
		var existing []map[string]interface{}
		if data, err := os.ReadFile(modelsPath); err == nil {
			json.Unmarshal(data, &existing)
		}
		var kept []map[string]interface{}
		for _, m := range existing {
			title, _ := m["title"].(string)
			if strings.Contains(strings.ToLower(title), "superkiro") || strings.Contains(strings.ToLower(title), "omniproxy") {
				continue
			}
			kept = append(kept, m)
		}
		data, _ := json.MarshalIndent(kept, "", "  ")
		os.WriteFile(modelsPath, data, 0644)
		delCliToolSettings("copilot")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	// GetAllAccountsFull returns ALL config accounts with live pool stats
	// overlaid. Accounts not in the pool (banned/disabled) retain their
	// last-persisted stats from config — so their token/request counters
	// show historical usage instead of 0.
	accounts := h.pool.GetAllAccountsFull()

	// hide sensitive info
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		result[i] = map[string]interface{}{
			"id":                        a.ID,
			"email":                     a.Email,
			"userId":                    a.UserId,
			"nickname":                  a.Nickname,
			"authMethod":                a.AuthMethod,
			"provider":                  a.Provider,
			"sourceId":                  a.SourceID,
			"providerKind":              a.ProviderKind,
			"capabilities":              a.Capabilities,
			"discoveredCapabilities":    a.DiscoveredCapabilities,
			"capabilitiesDiscoveredAt":  a.CapabilitiesDiscoveredAt,
			"capabilityProbes":          a.CapabilityProbes,
			"region":                    a.Region,
			"enabled":                   a.Enabled,
			"banStatus":                 a.BanStatus,
			"banReason":                 a.BanReason,
			"banTime":                   a.BanTime,
			"expiresAt":                 a.ExpiresAt,
			"hasToken":                  a.AccessToken != "",
			"machineId":                 a.MachineId,
			"weight":                    a.Weight,
			"overageStatus":             a.OverageStatus,
			"overageCapability":         a.OverageCapability,
			"overageCap":                a.OverageCap,
			"overageRate":               a.OverageRate,
			"currentOverages":           a.CurrentOverages,
			"overageCheckedAt":          a.OverageCheckedAt,
			"proxyURL":                  a.ProxyURL,
			"subscriptionType":          a.SubscriptionType,
			"subscriptionTitle":         a.SubscriptionTitle,
			"daysRemaining":             a.DaysRemaining,
			"usageCurrent":              a.UsageCurrent,
			"usageLimit":                a.UsageLimit,
			"usagePercent":              a.UsagePercent,
			"nextResetDate":             a.NextResetDate,
			"lastRefresh":               a.LastRefresh,
			"trialUsageCurrent":         a.TrialUsageCurrent,
			"trialUsageLimit":           a.TrialUsageLimit,
			"trialUsagePercent":         a.TrialUsagePercent,
			"trialStatus":               a.TrialStatus,
			"trialExpiresAt":            a.TrialExpiresAt,
			"requestCount":              a.RequestCount,
			"errorCount":                a.ErrorCount,
			"totalTokens":               a.TotalTokens,
			"totalCredits":              a.TotalCredits,
			"lastUsed":                  a.LastUsed,
			"serviceRequestCount":       a.ServiceRequestCount,
			"serviceErrorCount":         a.ServiceErrorCount,
			"serviceQuotaErrorCount":    a.ServiceQuotaErrorCount,
			"serviceLastUsed":           a.ServiceLastUsed,
			"serviceLastStatus":         a.ServiceLastStatus,
			"serviceRateLimit":          a.ServiceRateLimit,
			"serviceRateLimitRemaining": a.ServiceRateLimitRemaining,
			"serviceRateLimitReset":     a.ServiceRateLimitReset,
			"serviceRetryAfter":         a.ServiceRetryAfter,
			"serviceUsageCheckedAt":     a.ServiceUsageCheckedAt,
			"baseUrl":                   a.BaseURL,
			"extCreditLimit":            a.ExtCreditLimit,
			"extCreditsRemaining":       a.ExtCreditsRemaining,
			"extCreditsUsed":            a.ExtCreditsUsed,
			"extRequestsCount":          a.ExtRequestsCount,
			"extTokensUsed":             a.ExtTokensUsed,
			"extStatus":                 a.ExtStatus,
			"extKeyMasked":              a.ExtKeyMasked,
			"extLastUsedAt":             a.ExtLastUsedAt,
			"extCreditsCheckedAt":       a.ExtCreditsCheckedAt,
			"chatgptAccountId":          a.ChatGPTAccountID,
			"codexPlanType":             a.CodexPlanType,
			"codexActiveLimit":          a.CodexActiveLimit,
			"codexEmail":                a.CodexEmail,
			"codexName":                 a.CodexName,
			"codexPrimaryUsedPercent":   a.CodexPrimaryUsedPercent,
			"codexSecondaryUsedPercent": a.CodexSecondaryUsedPercent,
			"codexPrimaryWindowMinutes": a.CodexPrimaryWindowMinutes,
			"codexPrimaryResetAt":       a.CodexPrimaryResetAt,
			"codexSecondaryResetAt":     a.CodexSecondaryResetAt,
			"codexCreditsBalance":       a.CodexCreditsBalance,
			"codexCreditsUnlimited":     a.CodexCreditsUnlimited,
			"codexCreditsKnown":         a.CodexCreditsKnown,
			// Always emitted (even when 0) so the UI can show the real count
			// and disable the Bank Reset button instead of hiding it.
			"codexResetCreditsAvailable": a.CodexResetCreditsAvailable,
			"codexUsageCheckedAt":        a.CodexUsageCheckedAt,
			"imageModel":                a.ImageModel,
			"codexImageModel":           a.CodexImageModel,
			"tokenRefreshedAt":          a.TokenRefreshedAt,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}
	if account.MachineId == "" {
		account.MachineId = config.GenerateMachineId()
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// if new account is enabled with token, immediately fetch and cache model list
	if account.Enabled && account.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// get existing account
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// only update provided fields
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
		// When re-enabling an account, clear any prior ban/suspend marker so
		// the UI reflects the operator's intent and the pool routes to it again.
		if v && (existing.BanStatus == "BANNED" || existing.BanStatus == "DISABLED" || existing.BanStatus == "SUSPENDED" || existing.BanStatus == codexReauthRequiredStatus) {
			existing.BanStatus = "ACTIVE"
			existing.BanReason = ""
			existing.BanTime = 0
		}
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}
	// External OpenAI-compatible provider editable fields.
	if v, ok := updates["baseUrl"].(string); ok {
		existing.BaseURL = strings.TrimSpace(v)
	}
	if v, ok := updates["accessToken"].(string); ok {
		existing.AccessToken = strings.TrimSpace(v)
	}
	if v, ok := updates["codexImageModel"].(string); ok && isCodexAccount(existing) {
		existing.CodexImageModel = strings.TrimSpace(v)
	}
	if v, ok := updates["imageModel"].(string); ok {
		existing.ImageModel = strings.TrimSpace(v)
	}
	// Per-account cache_control passthrough override. Tri-state on the wire:
	// a bool sets an explicit override, JSON null clears it back to
	// "inherit the global setting". Distinguishing null from absent matters —
	// an absent key must leave the current override untouched, otherwise every
	// unrelated account edit from the UI would silently reset the canary.
	if raw, present := updates["cacheControlPassthrough"]; present {
		switch v := raw.(type) {
		case bool:
			flag := v
			existing.CacheControlPassthrough = &flag
		case nil:
			existing.CacheControlPassthrough = nil
		}
	}

	_, changesCredentials := updates["accessToken"]
	var persistErr error
	if changesCredentials {
		persistErr = config.UpdateAccount(id, *existing)
	} else {
		persistErr = config.UpdateAccountPreservingCredentials(id, *existing)
	}
	if persistErr != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": persistErr.Error()})
		return
	}

	h.pool.Reload()
	// when account goes from disabled to enabled, auto-fetch and cache model list
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage fetches and returns upstream Overages status for a single account.
// Synchronously writes the result back to config.json cache, ensuring UI and persistence are consistent.
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Overages is a Kiro/AWS subscription concept — not applicable to
	// external OpenAI-compatible providers or Codex (ChatGPT subscription)
	// accounts. Calling FetchOverageStatus with their access token hits
	// q.external.amazonaws.com and fails with a DNS error. Return a
	// friendly error instead.
	if isExternalAccount(account) || isCodexAccount(account) || isAntigravityAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Overages are only available for native Kiro/AWS accounts"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage toggles upstream Overages for a single account and refreshes cache.
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Overages is a Kiro/AWS subscription concept — not applicable to
	// external OpenAI-compatible providers or Codex (ChatGPT subscription)
	// accounts. Refuse the toggle so the UI doesn't trigger a doomed
	// q.external.amazonaws.com call.
	if isExternalAccount(account) || isCodexAccount(account) || isAntigravityAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Overages are only available for native Kiro/AWS accounts"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts batch-operates accounts (enable/disable/refresh)
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// record accounts going from disabled to enabled with token
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				if err := config.UpdateAccountPreservingCredentials(a.ID, a); err != nil {
					logger.Errorf("[apiBatchAccounts] Failed to persist %s for %s: %v", req.Action, a.Email, err)
				}
			}
		}
		h.pool.Reload()
		// Asynchronously fetches model cache for newly enabled accounts
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// Codex accounts: refresh JWT profile + seed models, skip Kiro API
			if isCodexAccount(account) {
				refreshCodexAccountID(account)
				h.fetchAndCacheAccountModels(account)
				successCount++
				continue
			}
			// External OpenAI-compatible providers: refresh models + credits
			if isExternalAccount(account) {
				h.fetchAndCacheAccountModels(account)
				h.refreshExternalCredits(account)
				successCount++
				continue
			}
			if isServiceAccount(account) {
				successCount++
				continue
			}
			// refresh token
			if account.RefreshToken != "" {
				if newAccess, newRefresh, newExpires, profileArn, _, _, err := auth.RefreshAccountToken(account); err == nil {
					account.AccessToken = newAccess
					if newRefresh != "" {
						account.RefreshToken = newRefresh
					}
					account.ExpiresAt = newExpires
					config.UpdateAccountToken(id, newAccess, newRefresh, newExpires)
					if profileArn != "" {
						account.ProfileArn = profileArn
						config.UpdateAccountProfileArn(id, profileArn)
					}
					h.pool.UpdateToken(id, newAccess, newRefresh, newExpires)
				}
			}
			// refresh account info
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl   string `json:"startUrl"`
		Region     string `json:"region"`
		ProfileArn string `json:"profileArn,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, profileArn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// get user info
	email, _, _ := auth.GetUserInfo(accessToken)

	// create account
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region     string `json:"region"`
		ProfileArn string `json:"profileArn,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, profileArn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// Declared here (from PollBuilderIdAuth) but only used in the completed branch below.
	// Suppress the Go unused-variable warning for the early-return (pending) path.
	_ = profileArn

	if status == "pending" || status == "slow_down" {
		// get current interval
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// Authorization complete, get user info
	email, _, _ := auth.GetUserInfo(accessToken)

	// create account
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
		ProfileArn  string `json:"profileArn,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// supports batch import, split by line
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, profileArn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// Social tokens from kiro.dev can't call CodeWhisperer getUsageLimits,
		// so extract email from JWT claims instead.
		email := auth.ExtractEmailFromJWT(accessToken)

		// create account
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
			ProfileArn:   profileArn,
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

// ── Kiro SSO (Browser-Based Social/Enterprise Login) ─────────────────────────

func (h *Handler) apiKiroSsoStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	region := req.Region
	if region == "" {
		region = "us-east-1"
	}

	session, err := auth.NewSsoSession(region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	signInURL := auth.SocialSignInURL(session.PKCE)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"authUrl":   signInURL,
		"state":     session.PKCE.State,
		"region":    region,
	})
}

func (h *Handler) apiKiroEnterpriseStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	session := auth.GetSsoSession(req.SessionID)
	if session == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Session not found or expired"})
		return
	}

	parsed, err := url.Parse(req.CallbackURL)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid callback URL"})
		return
	}
	q := parsed.Query()
	issuerURL := strings.TrimSpace(q.Get("issuer_url"))
	clientID2 := strings.TrimSpace(q.Get("client_id"))
	scopes := strings.TrimSpace(q.Get("scopes"))
	loginHint := strings.TrimSpace(q.Get("login_hint"))
	logger.Infof("[SSO-Enterprise] descriptor: issuer=%s client_id=%s scopes=%q login_hint=%q", issuerURL, clientID2, scopes, loginHint)
	if clientID2 == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Missing client_id in enterprise descriptor"})
		return
	}

	authEndpoint, tokenEndpoint, err := auth.ExternalIdpDiscover(r.Context(), issuerURL)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	pkce2, err := auth.GenerateSocialPKCE()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	redirectURI := "http://localhost:3128/oauth/callback"
	idpAuthURL := auth.BuildExternalIdpAuthorizeURL(authEndpoint, clientID2, redirectURI, scopes, pkce2.Challenge, pkce2.State, loginHint)
	logger.Infof("[SSO-Enterprise] IdP auth URL built, redirect_uri=%s", redirectURI)

	auth.SetSsoEnterpriseContext(req.SessionID, tokenEndpoint, issuerURL, clientID2, scopes, pkce2.Verifier, redirectURI)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"idpAuthUrl": idpAuthURL,
		"state":      pkce2.State,
	})
}

func (h *Handler) apiKiroSsoExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	session := auth.GetSsoSession(req.SessionID)
	if session == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Session not found or expired"})
		return
	}
	defer auth.DeleteSsoSession(req.SessionID)

	parsed, err := url.Parse(req.CallbackURL)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid callback URL"})
		return
	}
	q := parsed.Query()
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	errParam := strings.TrimSpace(q.Get("error"))
	desc := strings.TrimSpace(q.Get("error_description"))
	if errParam != "" {
		msg := errParam
		if desc != "" {
			msg += ": " + desc
		}
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	if code == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No authorization code in callback"})
		return
	}

	// Validate state for enterprise leg-2 (anti-CSRF).
	if session.Provider == "enterprise" && session.CodeVerifier != "" && state != "" && state != session.ID {
		logger.Warnf("[SSO-Exchange] state mismatch: expected session=%s got=%s", session.ID, state)
	}

	var accessToken, refreshToken, profileArn string
	var expiresIn int
	region := session.Region
	if region == "" {
		region = "us-east-1"
	}

	if session.Provider == "enterprise" {
		if session.CodeVerifier == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Missing enterprise PKCE verifier"})
			return
		}
		logger.Infof("[SSO-Exchange] enterprise: token_endpoint=%s client_id=%s scopes=%q redirect_uri=%s", session.TokenEndpoint, session.ClientID, session.Scopes, session.RedirectURI)
		at, rt, exp, err := auth.ExchangeExternalIdpCode(r.Context(), session.TokenEndpoint, session.ClientID, code, session.CodeVerifier, session.RedirectURI, session.Scopes)
		if err != nil {
			logger.Errorf("[SSO-Exchange] enterprise exchange failed: %v", err)
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		accessToken, refreshToken, expiresIn = at, rt, exp
		logger.Infof("[SSO-Exchange] enterprise success: at_len=%d rt_len=%d exp=%d", len(accessToken), len(refreshToken), expiresIn)
	} else {
		if session.PKCE == nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Missing PKCE data"})
			return
		}
		at, rt, pa, exp, err := auth.ExchangeSocialCode(r.Context(), code, session.PKCE.Verifier)
		if err != nil {
			logger.Errorf("[SSO-Exchange] social exchange failed: %v", err)
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		accessToken, refreshToken, profileArn, expiresIn = at, rt, pa, exp
	}

	// Determine auth method and IdP context from session.
	authMethod := "social"
	var tokenEndpoint, issuerURL, scopesStr string
	if session.Provider == "enterprise" {
		authMethod = "external_idp"
		tokenEndpoint = session.TokenEndpoint
		issuerURL = session.IssuerURL
		scopesStr = session.Scopes
	}

	newAccessToken := accessToken
	newRefreshToken := refreshToken
	newExpiresAt := time.Now().Unix() + int64(expiresIn)
	if profileArn == "" {
		pa, newAT, newRT, newExp, resolveErr := auth.ResolveProfileArn(accessToken, region, session.ClientID, "", tokenEndpoint, scopesStr, refreshToken)
		if resolveErr == nil {
			if pa != "" {
				profileArn = pa
			}
			if newAT != "" {
				newAccessToken = newAT
				newRefreshToken = newRT
				newExpiresAt = newExp
			}
		}
	}

	// Enterprise (external IdP) access tokens are IdP-issued JWTs (Azure AD), not AWS
	// Cognito tokens. CodeWhisperer REST APIs like GetUserInfo reject them, so extract
	// the email from the JWT claims instead.
	var email string
	if authMethod == "external_idp" {
		email = auth.ExtractEmailFromJWT(newAccessToken)
	} else {
		email, _, _ = auth.GetUserInfo(newAccessToken)
	}

	if profileArn != "" {
		existing := findDedupTarget(profileArn, email, authMethod)
		if existing != nil {
			existing.AccessToken = newAccessToken
			existing.RefreshToken = newRefreshToken
			existing.ExpiresAt = newExpiresAt
			existing.Email = email
			existing.AuthMethod = authMethod
			existing.Enabled = true
			existing.BanStatus = "ACTIVE"
			existing.BanReason = ""
			existing.BanTime = 0
			if tokenEndpoint != "" {
				existing.TokenEndpoint = tokenEndpoint
				existing.IssuerURL = issuerURL
				existing.Scopes = scopesStr
			}
			if err := config.UpdateAccount(existing.ID, *existing); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			h.pool.Reload()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"source":  "kiro-sso-" + session.Provider,
				"account": map[string]interface{}{
					"id":    existing.ID,
					"email": email,
				},
			})
			return
		}
	}

	// Fallback: try to find existing account by email to avoid duplicates
	// when profileArn was unavailable (e.g. gateway was down during resolution).
	if email != "" {
		if existing := config.FindAccountByEmail(email); existing != nil {
			existing.AccessToken = newAccessToken
			existing.RefreshToken = newRefreshToken
			existing.ExpiresAt = newExpiresAt
			existing.Enabled = true
			existing.BanStatus = "ACTIVE"
			existing.BanReason = ""
			existing.BanTime = 0
			existing.ProfileArn = profileArn
			existing.AuthMethod = authMethod
			if tokenEndpoint != "" {
				existing.TokenEndpoint = tokenEndpoint
				existing.IssuerURL = issuerURL
				existing.Scopes = scopesStr
			}
			if err := config.UpdateAccount(existing.ID, *existing); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			h.pool.Reload()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"source":  "kiro-sso-" + session.Provider,
				"account": map[string]interface{}{
					"id":    existing.ID,
					"email": email,
				},
			})
			return
		}
	}

	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         email,
		AccessToken:   newAccessToken,
		RefreshToken:  newRefreshToken,
		AuthMethod:    authMethod,
		Provider:      "BuilderId",
		Region:        region,
		ExpiresAt:     newExpiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		ProfileArn:    profileArn,
		TokenEndpoint: tokenEndpoint,
		IssuerURL:     issuerURL,
		Scopes:        scopesStr,
	}
	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  "kiro-sso-" + session.Provider,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": email,
		},
	})
}

func (h *Handler) apiSocialLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Provider != "google" && req.Provider != "github" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid provider. Use 'google' or 'github'"})
		return
	}

	session, err := auth.StartSocialLogin(req.Provider)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"authUrl":    session.VerifyURL,
		"deviceCode": session.DeviceCode,
		"userCode":   session.UserCode,
		"expiresIn":  int(time.Until(session.ExpiresAt).Seconds()),
		"interval":   session.Interval,
		"provider":   req.Provider,
	})
}

func (h *Handler) apiSocialLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"deviceCode"`
		Provider   string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, profileArn, expiresIn, err := auth.PollSocialLogin(req.DeviceCode, req.Provider)
	if err != nil {
		errMsg := err.Error()
		if errMsg == "authorization_pending" || errMsg == "slow_down" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"pending": true,
				"error":   errMsg,
			})
			return
		}
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	// get user info
	email, _, _ := auth.GetUserInfo(accessToken)

	providerName := "Google"
	if req.Provider == "github" {
		providerName = "Github"
	}
	authMethod := "social"

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		AuthMethod:   authMethod,
		Provider:     providerName,
		Region:       "us-east-1",
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// ==================== Codex (ChatGPT subscription) OAuth ====================

// apiCodexLoginStart begins a Codex PKCE login flow and opens its authorize
// URL in a separate Chrome profile. The proxy starts a local HTTP server on
// port 1455 to receive the OAuth callback.
func (h *Handler) apiCodexLoginStart(w http.ResponseWriter, r *http.Request) {
	session, err := auth.StartCodexLogin()
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	browserErr := auth.OpenCodexLoginInCleanBrowser()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authUrl":         session.AuthURL,
		"expiresIn":       int(time.Until(session.ExpiresAt).Seconds()),
		"provider":        "codex",
		"browserLaunched": browserErr == nil,
		"browserError":    errorMessage(browserErr),
	})
}

// codexBrowserProfileDir derives the only persistent browser profile path for
// a local Codex account. Hashing keeps an account ID from becoming a path
// component and prevents a caller from selecting arbitrary directories.
func codexBrowserProfileDir(accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return filepath.Join(config.GetConfigDir(), "codex-browser-profiles", fmt.Sprintf("%x", sum[:]))
}

// quarantineCodexBrowserProfile preserves an unverified browser profile
// outside the active profile path. It avoids silently deleting an OpenAI
// session if setup was interrupted before the successful OAuth callback could
// record the account binding.
func quarantineCodexBrowserProfile(profileDir string) (string, error) {
	info, err := os.Stat(profileDir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Codex browser profile path is not a directory")
	}

	quarantineDir := filepath.Join(filepath.Dir(profileDir), "unverified")
	if err := os.MkdirAll(quarantineDir, 0700); err != nil {
		return "", err
	}
	archived := filepath.Join(quarantineDir, filepath.Base(profileDir)+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(profileDir, archived); err != nil {
		return "", err
	}
	return archived, nil
}

// apiOpenCodexSecurity opens the Security page with the account-scoped Chrome
// profile. A profile becomes usable only after an OAuth callback proves it is
// authenticated as this exact ChatGPT account.
func (h *Handler) apiOpenCodexSecurity(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !isCodexAccount(account) || strings.TrimSpace(account.ChatGPTAccountID) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account is not a linked Codex account"})
		return
	}

	profileDir := codexBrowserProfileDir(account.ID)
	if account.CodexBrowserProfileVerified &&
		account.CodexBrowserProfileAccountID == account.ChatGPTAccountID {
		info, err := os.Stat(profileDir)
		if err == nil && info.IsDir() {
			if err := auth.OpenCodexSecurityProfile(profileDir); err != nil {
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":        true,
				"securityOpened": true,
				"message":        "Opened ChatGPT Security in this account's browser profile",
			})
			return
		}
		if err != nil && !os.IsNotExist(err) {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Could not inspect Codex browser profile: " + err.Error()})
			return
		}
	}

	// Preserve a stale profile from an interrupted setup rather than deleting
	// its browser session. The fresh OAuth callback below is the binding proof
	// for the newly created active profile.
	if _, err := quarantineCodexBrowserProfile(profileDir); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not preserve previous Codex browser profile: " + err.Error()})
		return
	}
	session, err := auth.StartCodexLoginForAccount(profileDir, account.ID)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	browserErr := auth.OpenCodexLoginInCleanBrowser()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"setupRequired":   true,
		"authUrl":         session.AuthURL,
		"expiresIn":       int(time.Until(session.ExpiresAt).Seconds()),
		"browserLaunched": browserErr == nil,
		"browserError":    errorMessage(browserErr),
		"message":         "Sign in to the Codex account shown on this card to link its browser profile",
	})
}

// apiCodexLoginOpenBrowser reopens the current authorize URL in another
// isolated Chrome profile. This is a recovery path when the first window was
// closed before the OAuth callback completed.
func (h *Handler) apiCodexLoginOpenBrowser(w http.ResponseWriter, r *http.Request) {
	if err := auth.OpenCodexLoginInCleanBrowser(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "browserLaunched": true})
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// upsertCodexOAuthAccount applies a successful interactive login or explicit
// token import. A ChatGPT account must have exactly one OmniProxy account: two
// copies would hold the same rotating refresh token and can invalidate each
// other when they refresh concurrently.
func upsertCodexOAuthAccount(candidate config.Account, nicknameExplicit bool) (config.Account, bool, error) {
	for _, existing := range config.GetAccounts() {
		if existing.AuthMethod != codexAuthMethod || existing.ChatGPTAccountID != candidate.ChatGPTAccountID {
			continue
		}

		// Keep the stable local identity and account-specific settings. OAuth is
		// an intentional credential replacement, so these fields are updated as
		// one durable config write before the caller reloads the pool.
		existing.AccessToken = candidate.AccessToken
		existing.RefreshToken = candidate.RefreshToken
		existing.ExpiresAt = candidate.ExpiresAt
		existing.TokenRefreshedAt = time.Now().Unix()
		existing.Email = candidate.Email
		existing.AuthMethod = codexAuthMethod
		existing.Provider = candidate.Provider
		existing.ChatGPTAccountID = candidate.ChatGPTAccountID
		existing.Region = candidate.Region
		existing.Enabled = true
		existing.BanStatus = "ACTIVE"
		existing.BanReason = ""
		existing.BanTime = 0
		if nicknameExplicit || existing.Nickname == "" {
			existing.Nickname = candidate.Nickname
		}
		if candidate.CodexEmail != "" {
			existing.CodexEmail = candidate.CodexEmail
		}
		if candidate.CodexName != "" {
			existing.CodexName = candidate.CodexName
		}
		if candidate.CodexPlanType != "" {
			existing.CodexPlanType = candidate.CodexPlanType
		}
		if err := config.UpdateAccount(existing.ID, existing); err != nil {
			return config.Account{}, false, err
		}
		return existing, false, nil
	}

	if err := config.AddAccount(candidate); err != nil {
		return config.Account{}, false, err
	}
	return candidate, true, nil
}

// apiCodexLoginPoll polls the active Codex login session. On success, it
// creates or updates a Codex account with the access/refresh tokens and
// chatgpt_account_id extracted from the JWT.
func (h *Handler) apiCodexLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nickname string `json:"nickname"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	targetAccountID := auth.CurrentCodexLoginTargetAccountID()
	tokens, err := auth.PollCodexLogin()
	if err != nil {
		errMsg := err.Error()
		if errMsg == "authorization_pending" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"pending": true,
				"error":   errMsg,
			})
			return
		}
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}
	acceptedLogin := false
	defer func() {
		if acceptedLogin {
			auth.CompleteCodexLogin()
			return
		}
		// An invalid callback result must terminate the persistent browser
		// process before its account-scoped profile is discarded below.
		auth.CancelCodexLogin()
	}()

	if targetAccountID != "" {
		accounts := config.GetAccounts()
		var target *config.Account
		for i := range accounts {
			if accounts[i].ID == targetAccountID {
				target = &accounts[i]
				break
			}
		}
		if target == nil || !isCodexAccount(target) || target.ChatGPTAccountID != tokens.AccountID {
			// The browser must exit before moving its profile, otherwise Chrome can
			// recreate files while the profile is being quarantined.
			auth.CancelCodexLogin()
			if _, err := quarantineCodexBrowserProfile(codexBrowserProfileDir(targetAccountID)); err != nil {
				logger.Warnf("[Codex] Failed to preserve mismatched browser profile: %v", err)
			}
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "The signed-in ChatGPT account does not match the selected Codex account; its browser profile was preserved outside the active profile path"})
			return
		}
	}

	// Extract profile info (email, name, plan_type) from the JWT.
	jwtInfo := auth.ExtractCodexJWTInfoPublic(tokens.AccessToken)

	// Use the JWT email if available; otherwise fall back to the
	// chatgpt_account_id prefix as the display name.
	displayName := "codex-" + tokens.AccountID[:min(8, len(tokens.AccountID))]
	nickname := "Codex (" + tokens.AccountID + ")"
	if jwtInfo.Email != "" {
		displayName = jwtInfo.Email
	}
	if jwtInfo.Name != "" {
		nickname = jwtInfo.Name
	}
	if requestedName := strings.TrimSpace(req.Nickname); requestedName != "" {
		nickname = requestedName
	}

	account := config.Account{
		ID:               auth.GenerateAccountID(),
		Email:            displayName,
		Nickname:         nickname,
		AuthMethod:       codexAuthMethod,
		Provider:         "OpenAI Codex",
		AccessToken:      tokens.AccessToken,
		RefreshToken:     tokens.RefreshToken,
		ExpiresAt:        tokens.ExpiresAt,
		ChatGPTAccountID: tokens.AccountID,
		Region:           "external",
		Enabled:          true,
		MachineId:        config.GenerateMachineId(),
		CodexEmail:       jwtInfo.Email,
		CodexName:        jwtInfo.Name,
		CodexPlanType:    jwtInfo.PlanType,
	}

	account, _, err = upsertCodexOAuthAccount(account, strings.TrimSpace(req.Nickname) != "")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if targetAccountID != "" {
		account.CodexBrowserProfileVerified = true
		account.CodexBrowserProfileAccountID = tokens.AccountID
		if err := config.UpdateAccount(account.ID, account); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Could not persist Codex browser profile: " + err.Error()})
			return
		}
	}
	h.pool.Reload()
	acceptedLogin = true
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"profileLinked":  targetAccountID != "",
		"securityOpened": targetAccountID != "",
		"account": map[string]interface{}{
			"id":               account.ID,
			"email":            account.Email,
			"chatgptAccountId": account.ChatGPTAccountID,
			"planType":         jwtInfo.PlanType,
			"name":             jwtInfo.Name,
			"expiresAt":        account.ExpiresAt,
		},
	})
}

// apiCodexLoginCancel tears down any active Codex login session. Useful
// when the user abandons the browser flow or wants to restart.
func (h *Handler) apiCodexLoginCancel(w http.ResponseWriter, r *http.Request) {
	auth.CancelCodexLogin()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiImportCodexTokens lets an operator import a pre-existing Codex
// auth.json (e.g. from ~/.codex/auth.json on another machine) instead of
// running the OAuth flow. The access token's JWT is decoded to extract
// the chatgpt_account_id.
func (h *Handler) apiImportCodexTokens(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Nickname     string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.AccessToken) == "" || strings.TrimSpace(req.RefreshToken) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "accessToken and refreshToken are required"})
		return
	}

	accountID := extractCodexAccountIDForImport(req.AccessToken)
	if accountID == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "access token JWT missing chatgpt_account_id (not a Codex token?)"})
		return
	}

	// Extract profile info from JWT
	jwtInfo := auth.ExtractCodexJWTInfoPublic(strings.TrimSpace(req.AccessToken))

	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = "Codex (" + accountID + ")"
		if jwtInfo.Name != "" {
			nickname = jwtInfo.Name
		}
	}
	displayEmail := "codex-" + accountID[:min(8, len(accountID))]
	if jwtInfo.Email != "" {
		displayEmail = jwtInfo.Email
	}

	account := config.Account{
		ID:               auth.GenerateAccountID(),
		Email:            displayEmail,
		Nickname:         nickname,
		AuthMethod:       codexAuthMethod,
		Provider:         "OpenAI Codex",
		AccessToken:      strings.TrimSpace(req.AccessToken),
		RefreshToken:     strings.TrimSpace(req.RefreshToken),
		ExpiresAt:        req.ExpiresAt,
		ChatGPTAccountID: accountID,
		Region:           "external",
		Enabled:          true,
		MachineId:        config.GenerateMachineId(),
		CodexEmail:       jwtInfo.Email,
		CodexName:        jwtInfo.Name,
		CodexPlanType:    jwtInfo.PlanType,
	}
	if account.ExpiresAt == 0 {
		// Default 1h if caller didn't supply; refresh will fix it up.
		account.ExpiresAt = time.Now().Unix() + 3600
	}

	account, _, err := upsertCodexOAuthAccount(account, strings.TrimSpace(req.Nickname) != "")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":               account.ID,
			"email":            account.Email,
			"nickname":         account.Nickname,
			"chatgptAccountId": account.ChatGPTAccountID,
			"planType":         jwtInfo.PlanType,
			"name":             jwtInfo.Name,
			"expiresAt":        account.ExpiresAt,
		},
	})
}

// ==================== 9router import ====================

// apiPreview9Router reads ~/.9router/db.json and returns a preview of the
// codex + kiro accounts that would be imported, WITHOUT writing anything.
// The frontend uses this to show a confirmation modal before committing.
func (h *Handler) apiPreview9Router(w http.ResponseWriter, r *http.Request) {
	result, err := auth.ReadNineRouterDB()
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"path":      result.Path,
		"codex":     nineRouterAccountsToJSON(result.Codex),
		"kiro":      nineRouterAccountsToJSON(result.Kiro),
		"providers": nineRouterAccountsToJSON(result.Generic),
		"skipped":   result.Skipped,
	})
}

// apiImportFrom9Router reads ~/.9router/db.json and imports all codex +
// kiro accounts into OmniProxy's config. Codex accounts are imported
// directly (their access token is a JWT we can decode for the
// chatgpt_account_id). Kiro accounts go through token refresh to validate
// + extract profileArn, mirroring the kiro-cli import path.
//
// Request body (all optional):
//   - importCodex: bool (default true)
//   - importKiro:  bool (default true)
//   - importProviders: bool (default true)
//   - codexSourceIds/kiroSourceIds/providerSourceIds: selected 9router IDs
//   - codexIndexes/kiroIndexes/providerIndexes: selected indexes for records
//     without a source ID
//   - refreshKiro: bool (default true — refresh kiro tokens to validate)
func (h *Handler) apiImportFrom9Router(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ImportCodex       *bool    `json:"importCodex"`
		ImportKiro        *bool    `json:"importKiro"`
		ImportProviders   *bool    `json:"importProviders"`
		ImportGeneric     *bool    `json:"importGeneric"`
		RefreshKiro       *bool    `json:"refreshKiro"`
		CodexSourceIDs    []string `json:"codexSourceIds"`
		KiroSourceIDs     []string `json:"kiroSourceIds"`
		ProviderSourceIDs []string `json:"providerSourceIds"`
		CodexIndexes      []int    `json:"codexIndexes"`
		KiroIndexes       []int    `json:"kiroIndexes"`
		ProviderIndexes   []int    `json:"providerIndexes"`
	}
	// Body is optional; ignore decode errors.
	_ = json.NewDecoder(r.Body).Decode(&req)

	importCodex := true
	importKiro := true
	importProviders := true
	refreshKiro := true
	if req.ImportCodex != nil {
		importCodex = *req.ImportCodex
	}
	if req.ImportKiro != nil {
		importKiro = *req.ImportKiro
	}
	if req.ImportProviders != nil {
		importProviders = *req.ImportProviders
	}
	if req.ImportGeneric != nil {
		importProviders = *req.ImportGeneric
	}
	if req.RefreshKiro != nil {
		refreshKiro = *req.RefreshKiro
	}

	result, err := auth.ReadNineRouterDB()
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	type importedAcc struct {
		Source       string   `json:"source"`
		Name         string   `json:"name"`
		AccountID    string   `json:"accountId,omitempty"`
		Email        string   `json:"email,omitempty"`
		PlanType     string   `json:"planType,omitempty"`
		ProviderKind string   `json:"providerKind,omitempty"`
		Capabilities []string `json:"capabilities,omitempty"`
		Status       string   `json:"status"` // "imported" | "skipped" | "error"
		Error        string   `json:"error,omitempty"`
	}
	var imported []importedAcc
	skippedCount := 0

	if importProviders {
		for i, generic := range result.Generic {
			if !nineRouterSelectionIncludes(generic, i, req.ProviderSourceIDs, req.ProviderIndexes) {
				continue
			}
			acc, err := h.importOne9RouterGeneric(generic)
			if err != nil {
				imported = append(imported, importedAcc{Source: generic.Provider, Name: generic.Name, ProviderKind: generic.ProviderKind, Capabilities: generic.Capabilities, Status: "error", Error: err.Error()})
				continue
			}
			imported = append(imported, importedAcc{Source: generic.Provider, Name: generic.Name, AccountID: acc.ID, ProviderKind: acc.ProviderKind, Capabilities: acc.Capabilities, Status: "imported"})
		}
	}

	// ── Codex accounts ──
	if importCodex {
		for i, c := range result.Codex {
			if !nineRouterSelectionIncludes(c, i, req.CodexSourceIDs, req.CodexIndexes) {
				continue
			}
			acc, err := h.importOne9RouterCodex(c)
			if err != nil {
				imported = append(imported, importedAcc{
					Source: "codex", Name: c.Name, Status: "error", Error: err.Error(),
				})
				continue
			}
			if acc == nil {
				skippedCount++
				imported = append(imported, importedAcc{
					Source: "codex", Name: c.Name, Status: "skipped",
				})
				continue
			}
			imported = append(imported, importedAcc{
				Source:    "codex",
				Name:      c.Name,
				AccountID: acc.ChatGPTAccountID,
				Email:     acc.Email,
				PlanType:  acc.CodexPlanType,
				Status:    "imported",
			})
		}
	}

	// ── Kiro accounts ──
	if importKiro {
		for i, k := range result.Kiro {
			if !nineRouterSelectionIncludes(k, i, req.KiroSourceIDs, req.KiroIndexes) {
				continue
			}
			acc, err := h.importOne9RouterKiro(k, refreshKiro)
			if err != nil {
				imported = append(imported, importedAcc{
					Source: "kiro", Name: k.Name, Status: "error", Error: err.Error(),
				})
				continue
			}
			if acc == nil {
				skippedCount++
				imported = append(imported, importedAcc{
					Source: "kiro", Name: k.Name, Status: "skipped",
				})
				continue
			}
			imported = append(imported, importedAcc{
				Source: "kiro",
				Name:   k.Name,
				Email:  acc.Email,
				Status: "imported",
			})
		}
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":          true,
		"path":             result.Path,
		"imported":         imported,
		"importedCount":    len(imported) - skippedCount,
		"skippedCount":     skippedCount,
		"skippedProviders": result.Skipped,
	})
}

// importOne9RouterCodex imports a single Codex account from 9router.
// Dedups by chatgpt_account_id (updates existing account if found).
// Returns nil if the account is skipped (e.g. empty tokens).
func (h *Handler) importOne9RouterCodex(c auth.NineRouterImportedAccount) (*config.Account, error) {
	if c.AccessToken == "" || c.RefreshToken == "" {
		return nil, nil
	}
	// Re-extract chatgpt_account_id from the JWT in case 9router's
	// providerSpecificData is stale.
	accountID := c.ChatGPTAccountID
	if accountID == "" {
		accountID = auth.ExtractCodexAccountIDPublic(c.AccessToken)
	}
	if accountID == "" {
		return nil, fmt.Errorf("cannot extract chatgpt_account_id from access token")
	}

	// Extract profile info (email, name, plan_type) from JWT.
	jwtInfo := auth.ExtractCodexJWTInfoPublic(c.AccessToken)

	// Prefer the stable 9router connection ID. This also handles a rotated
	// token whose claims are temporarily incomplete or changed.
	for _, existing := range config.GetAccounts() {
		if c.SourceID != "" && existing.SourceID == c.SourceID {
			existing.AccessToken = c.AccessToken
			existing.RefreshToken = c.RefreshToken
			existing.ExpiresAt = c.ExpiresAt
			if existing.ExpiresAt == 0 {
				existing.ExpiresAt = time.Now().Unix() + 3600
			}
			existing.ChatGPTAccountID = accountID
			if c.Name != "" {
				existing.Nickname = c.Name
			}
			if jwtInfo.Email != "" {
				existing.CodexEmail = jwtInfo.Email
			}
			if jwtInfo.Name != "" {
				existing.CodexName = jwtInfo.Name
			}
			if jwtInfo.PlanType != "" {
				existing.CodexPlanType = jwtInfo.PlanType
			}
			if err := config.UpdateAccount(existing.ID, existing); err != nil {
				return nil, err
			}
			return &existing, nil
		}
	}

	// Dedup by chatgpt_account_id.
	for _, existing := range config.GetAccounts() {
		if existing.AuthMethod == codexAuthMethod && existing.ChatGPTAccountID == accountID {
			existing.AccessToken = c.AccessToken
			existing.RefreshToken = c.RefreshToken
			existing.ExpiresAt = c.ExpiresAt
			if c.ExpiresAt == 0 {
				existing.ExpiresAt = time.Now().Unix() + 3600
			}
			if c.Name != "" {
				existing.Nickname = c.Name
			}
			// Update JWT-extracted profile fields
			if jwtInfo.Email != "" {
				existing.CodexEmail = jwtInfo.Email
			}
			if jwtInfo.Name != "" {
				existing.CodexName = jwtInfo.Name
			}
			if jwtInfo.PlanType != "" {
				existing.CodexPlanType = jwtInfo.PlanType
			}
			if err := config.UpdateAccount(existing.ID, existing); err != nil {
				return nil, err
			}
			return &existing, nil
		}
	}

	email := "codex-" + accountID[:min(8, len(accountID))]
	if jwtInfo.Email != "" {
		email = jwtInfo.Email
	}
	nickname := c.Name
	if nickname == "" {
		nickname = "Codex (" + accountID + ")"
		if jwtInfo.Name != "" {
			nickname = jwtInfo.Name
		}
	}
	planLabel := c.PlanType
	if jwtInfo.PlanType != "" {
		planLabel = jwtInfo.PlanType
	}
	if planLabel != "" {
		nickname = nickname + " [" + planLabel + "]"
	}

	acc := config.Account{
		ID:               auth.GenerateAccountID(),
		Email:            email,
		Nickname:         nickname,
		AuthMethod:       codexAuthMethod,
		Provider:         "OpenAI Codex (9router)",
		AccessToken:      c.AccessToken,
		RefreshToken:     c.RefreshToken,
		ExpiresAt:        c.ExpiresAt,
		ChatGPTAccountID: accountID,
		SourceID:         c.SourceID,
		Region:           "external",
		Enabled:          true,
		MachineId:        config.GenerateMachineId(),
		CodexEmail:       jwtInfo.Email,
		CodexName:        jwtInfo.Name,
		CodexPlanType:    jwtInfo.PlanType,
	}
	if acc.ExpiresAt == 0 {
		acc.ExpiresAt = time.Now().Unix() + 3600
	}
	if err := config.AddAccount(acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// importOne9RouterGeneric preserves valid 9router credentials and capability
// metadata. Providers without a native adapter remain disabled so they cannot
// accidentally enter the chat dispatcher.
func (h *Handler) importOne9RouterGeneric(c auth.NineRouterImportedAccount) (*config.Account, error) {
	credential := strings.TrimSpace(c.APIKey)
	if credential == "" {
		credential = strings.TrimSpace(c.AccessToken)
	}
	if credential == "" {
		return nil, fmt.Errorf("provider %s has no credential", c.Provider)
	}

	for _, existing := range config.GetAccounts() {
		if c.SourceID != "" && existing.SourceID == c.SourceID {
			existing.AccessToken = credential
			existing.RefreshToken = c.RefreshToken
			existing.ExpiresAt = c.ExpiresAt
			existing.Provider = c.Provider
			if c.Name != "" {
				existing.Nickname = c.Name
			}
			existing.AuthMethod = genericAuthMethod(c)
			existing.ProviderKind = c.ProviderKind
			existing.Capabilities = append([]string(nil), c.Capabilities...)
			existing.BaseURL = c.BaseURL
			existing.ProxyURL = c.ProxyURL
			existing.Enabled = len(c.Capabilities) > 0
			if err := config.UpdateAccount(existing.ID, existing); err != nil {
				return nil, err
			}
			return &existing, nil
		}
	}

	acc := config.Account{
		ID: auth.GenerateAccountID(), Email: c.Provider + "-9router", Nickname: c.Name,
		AuthMethod: genericAuthMethod(c), Provider: c.Provider, SourceID: c.SourceID,
		ProviderKind: c.ProviderKind, Capabilities: append([]string(nil), c.Capabilities...),
		AccessToken: credential, RefreshToken: c.RefreshToken, ExpiresAt: c.ExpiresAt,
		BaseURL: c.BaseURL, ProxyURL: c.ProxyURL, Enabled: len(c.Capabilities) > 0,
		MachineId: config.GenerateMachineId(), Region: "external",
	}
	if acc.Nickname == "" {
		acc.Nickname = c.Provider
	}
	if err := config.AddAccount(acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// genericAuthMethod keeps OpenAI-compatible 9router connections on the
// external OpenAI dispatcher. Service adapters such as search and image use
// their own capability handlers and must not be routed as chat accounts.
func genericAuthMethod(c auth.NineRouterImportedAccount) string {
	if strings.EqualFold(strings.TrimSpace(c.ProviderKind), "chat") {
		return externalAuthMethod
	}
	return "service_api_key"
}

// importOne9RouterKiro imports a single Kiro account from 9router.
// 9router's kiro connections store refreshToken + profileArn but NOT
// clientId/clientSecret, so we use the social refresh path (kiro.dev
// /refreshToken) to validate + get a fresh access token.
// Dedups by profileArn (updates existing account if found).
func (h *Handler) importOne9RouterKiro(k auth.NineRouterImportedAccount, refresh bool) (*config.Account, error) {
	if k.RefreshToken == "" {
		return nil, nil
	}

	accessToken := k.AccessToken
	refreshToken := k.RefreshToken
	expiresAt := k.ExpiresAt
	profileArn := k.ProfileArn
	region := "us-east-1"

	if refresh {
		// Validate via social refresh (no clientId/clientSecret needed).
		tempAccount := &config.Account{
			RefreshToken: k.RefreshToken,
			AuthMethod:   "social",
			Region:       region,
		}
		newAccess, newRefresh, newExp, newProfileArn, _, _, err := auth.RefreshAccountToken(tempAccount)
		if err != nil {
			return nil, fmt.Errorf("kiro token refresh failed: %v", err)
		}
		if newAccess != "" {
			accessToken = newAccess
		}
		if newRefresh != "" {
			refreshToken = newRefresh
		}
		if newExp > 0 {
			expiresAt = newExp
		}
		if newProfileArn != "" {
			profileArn = newProfileArn
		}
	}

	email := ""
	if refresh && accessToken != "" {
		email, _, _ = auth.GetUserInfo(accessToken)
	}

	// SourceID is the primary identity for a 9router connection. This path is
	// also what makes startup sync network-free and idempotent.
	for _, existing := range config.GetAccounts() {
		if k.SourceID == "" || existing.SourceID != k.SourceID {
			continue
		}
		if email == "" {
			email = existing.Email
		}
		existing.AccessToken = accessToken
		existing.RefreshToken = refreshToken
		existing.ExpiresAt = expiresAt
		existing.ProfileArn = profileArn
		if email != "" {
			existing.Email = email
		}
		if k.Name != "" {
			existing.Nickname = k.Name
		}
		if err := config.UpdateAccount(existing.ID, existing); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if email == "" {
		label := k.SourceID
		if label == "" {
			label = "account"
		}
		email = "kiro-9router-" + label[:min(8, len(label))]
	}

	// Dedup by profileArn.
	if profileArn != "" {
		if existing := findDedupTarget(profileArn, email, "idc"); existing != nil {
			existing.AccessToken = accessToken
			existing.RefreshToken = refreshToken
			existing.ExpiresAt = expiresAt
			existing.Email = email
			if k.Name != "" {
				existing.Nickname = k.Name
			}
			if err := config.UpdateAccount(existing.ID, *existing); err != nil {
				return nil, err
			}
			return existing, nil
		}
	}

	nickname := k.Name
	if nickname == "" {
		nickname = "Kiro (9router)"
	}

	acc := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		Nickname:     nickname,
		AuthMethod:   "social",
		Provider:     "Imported (9router)",
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Region:       region,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
		SourceID:     k.SourceID,
	}
	if acc.ExpiresAt == 0 {
		acc.ExpiresAt = time.Now().Unix() + 3600
	}
	if err := config.AddAccount(acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// nineRouterAccountsToJSON converts the parsed account slice to the JSON
// shape returned by the preview endpoint (no tokens — just metadata).
func nineRouterAccountsToJSON(accounts []auth.NineRouterImportedAccount) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(accounts))
	for i, a := range accounts {
		entry := map[string]interface{}{
			"index":        i,
			"sourceId":     a.SourceID,
			"name":         a.Name,
			"provider":     a.Provider,
			"hasToken":     a.AccessToken != "" || a.RefreshToken != "",
			"providerKind": a.ProviderKind,
			"capabilities": a.Capabilities,
		}
		if a.ChatGPTAccountID != "" {
			entry["chatgptAccountId"] = a.ChatGPTAccountID
		}
		if a.ProfileArn != "" {
			entry["profileArn"] = a.ProfileArn
		}
		if a.PlanType != "" {
			entry["planType"] = a.PlanType
		}
		if a.ExpiresAt > 0 {
			entry["expiresAt"] = a.ExpiresAt
		}
		out = append(out, entry)
	}
	return out
}

// nineRouterSelectionIncludes applies the new per-account selection contract.
// A nil source ID/index pair means the caller used the legacy boolean-only
// contract, so all records in the enabled group remain eligible. When an
// explicit selection is present, stable source IDs are preferred; indexes are
// a fallback only for records that do not have a source ID.
func nineRouterSelectionIncludes(account auth.NineRouterImportedAccount, index int, sourceIDs []string, indexes []int) bool {
	if sourceIDs == nil && indexes == nil {
		return true
	}
	if account.SourceID != "" {
		for _, sourceID := range sourceIDs {
			if strings.TrimSpace(sourceID) == account.SourceID {
				return true
			}
		}
		return false
	}
	for _, selectedIndex := range indexes {
		if selectedIndex == index {
			return true
		}
	}
	return false
}

func (h *Handler) apiImportKiroCli(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileContent string `json:"fileContent"`
		FileName    string `json:"fileName"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	var creds *auth.KiroCliCredentials
	var err error
	if body.FileContent != "" {
		creds, err = auth.ParseKiroCliFile(body.FileContent, body.Region)
	} else {
		creds, err = auth.ImportKiroCli()
		// Fall back to ~/.aws/sso/cache if SQLite not found (OmniRoute parity).
		if err != nil {
			ssoCreds, ssoErr := auth.ImportSSOCache()
			if ssoErr == nil && ssoCreds.RefreshToken != "" {
				creds = &auth.KiroCliCredentials{
					RefreshToken: ssoCreds.RefreshToken,
					Region:       body.Region,
					ExpiresAt:    time.Now().Add(1 * time.Hour),
				}
				err = nil
			}
		}
	}
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Validate via refresh. kiro-cli gives us clientId/clientSecret for OIDC refresh.
	region := creds.Region
	if region == "" {
		region = "us-east-1"
	}
	// Register OIDC client if SQLite didn't have one (OmniRoute parity).
	// SSO-cache fallback path always needs this.
	clientID := creds.ClientID
	clientSecret := creds.ClientSecret
	if clientID == "" || clientSecret == "" {
		newCID, newCS, regErr := auth.RegisterOIDCClient(region)
		if regErr == nil && newCID != "" {
			clientID = newCID
			clientSecret = newCS
		}
	}
	tempAccount := &config.Account{
		RefreshToken: creds.RefreshToken,
		AccessToken:  creds.AccessToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
	}
	newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, err := auth.RefreshAccountToken(tempAccount)
	if err != nil {
		// Fall back to social refresh path (no clientId/clientSecret needed).
		tempAccount2 := &config.Account{
			RefreshToken: creds.RefreshToken,
			AuthMethod:   "social",
			Region:       region,
		}
		newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, err = auth.RefreshAccountToken(tempAccount2)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}
	if newRefreshToken != "" {
		creds.RefreshToken = newRefreshToken
	}
	if newAccessToken != "" {
		creds.AccessToken = newAccessToken
	}
	// Prefer profileArn from refresh response, fall back to SQLite scan.
	if profileArn == "" {
		profileArn = creds.ProfileArn
	}

	// get user info
	email, _, _ := auth.GetUserInfo(creds.AccessToken)

	// Dedup by profileArn: update existing account if found (OmniRoute parity).
	if profileArn != "" {
		existing := findDedupTarget(profileArn, email, "idc")
		if existing != nil {
			existing.AccessToken = creds.AccessToken
			existing.RefreshToken = creds.RefreshToken
			existing.ExpiresAt = expiresAt
			existing.Email = email
			if err := config.UpdateAccount(existing.ID, *existing); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			h.pool.Reload()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"source":  "kiro-cli-sqlite",
				"account": map[string]interface{}{
					"id":    existing.ID,
					"email": email,
				},
			})
			return
		}
	}

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  "kiro-cli-sqlite",
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiAutoImportKiroCli scans the filesystem for kiro-cli SQLite and ~/.aws/sso/cache
// without requiring file upload. Mirrors OmniRoute's GET /api/oauth/kiro/auto-import.
func (h *Handler) apiAutoImportKiroCli(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		region = "us-east-1"
	}
	targetProvider := r.URL.Query().Get("targetProvider")
	if targetProvider != "amazon-q" {
		targetProvider = "kiro"
	}

	// Try kiro-cli SQLite first
	creds, err := auth.ImportKiroCli()
	source := "kiro-cli-sqlite"

	// Fall back to ~/.aws/sso/cache
	if err != nil {
		ssoCreds, ssoErr := auth.ImportSSOCache()
		if ssoErr == nil && ssoCreds.RefreshToken != "" {
			creds = &auth.KiroCliCredentials{
				RefreshToken: ssoCreds.RefreshToken,
				Region:       region,
				ExpiresAt:    time.Now().Add(1 * time.Hour),
			}
			source = "aws-sso-cache"
			err = nil
		}
	}
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"found":      false,
			"error":      "Kiro credentials not found. Run `kiro-cli login` then retry, or use Import Token.",
			"triedPaths": []string{"~/.local/share/kiro-cli/data.sqlite3", "~/.aws/sso/cache/"},
		})
		return
	}

	// Register OIDC client if SQLite didn't have one
	clientID := creds.ClientID
	clientSecret := creds.ClientSecret
	if clientID == "" || clientSecret == "" {
		newCID, newCS, regErr := auth.RegisterOIDCClient(region)
		if regErr == nil && newCID != "" {
			clientID = newCID
			clientSecret = newCS
		}
	}

	// Refresh token
	tempAccount := &config.Account{
		RefreshToken: creds.RefreshToken,
		AccessToken:  creds.AccessToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
	}
	newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, refreshErr := auth.RefreshAccountToken(tempAccount)
	if refreshErr != nil {
		// Fall back to social refresh
		tempAccount2 := &config.Account{
			RefreshToken: creds.RefreshToken,
			AuthMethod:   "social",
			Region:       region,
		}
		newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, refreshErr = auth.RefreshAccountToken(tempAccount2)
		if refreshErr != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + refreshErr.Error()})
			return
		}
	}
	if newRefreshToken != "" {
		creds.RefreshToken = newRefreshToken
	}
	if newAccessToken != "" {
		creds.AccessToken = newAccessToken
	}
	if profileArn == "" {
		profileArn = creds.ProfileArn
	}

	email, _, _ := auth.GetUserInfo(creds.AccessToken)
	connectionName := auth.DeriveKiroConnectionName(email, profileArn, region, targetProvider)

	// Dedup by profileArn
	if profileArn != "" {
		existing := findDedupTarget(profileArn, email, "idc")
		if existing != nil {
			existing.AccessToken = creds.AccessToken
			existing.RefreshToken = creds.RefreshToken
			existing.ExpiresAt = expiresAt
			existing.Email = email
			existing.Nickname = connectionName
			if err := config.UpdateAccount(existing.ID, *existing); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			h.pool.Reload()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"found":  true,
				"source": source,
				"account": map[string]interface{}{
					"id":    existing.ID,
					"email": email,
				},
			})
			return
		}
	}

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		Nickname:     connectionName,
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"found":  true,
		"source": source,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiImportKiroToken accepts a pasted refresh token and validates/saves it.
// Mirrors OmniRoute's POST /api/oauth/kiro/import.
func (h *Handler) apiImportKiroToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refreshToken"`
		Region       string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	refreshToken := strings.TrimSpace(body.RefreshToken)
	if refreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}
	if !strings.HasPrefix(refreshToken, "aorAAAAAG") {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid token format. Token should start with aorAAAAAG..."})
		return
	}

	region := strings.TrimSpace(body.Region)
	if region == "" {
		region = "us-east-1"
	}
	targetProvider := r.URL.Query().Get("targetProvider")
	if targetProvider != "amazon-q" {
		targetProvider = "kiro"
	}

	// Register OIDC client for this connection
	clientID, clientSecret, regErr := auth.RegisterOIDCClient(region)
	if regErr != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to register OIDC client: " + regErr.Error()})
		return
	}

	// Try OIDC refresh first
	tempAccount := &config.Account{
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
	}
	newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, err := auth.RefreshAccountToken(tempAccount)
	if err != nil {
		// Fall back to social refresh
		tempAccount2 := &config.Account{
			RefreshToken: refreshToken,
			AuthMethod:   "social",
			Region:       region,
		}
		newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, err = auth.RefreshAccountToken(tempAccount2)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token validation failed: " + err.Error()})
			return
		}
	}
	if newRefreshToken != "" {
		refreshToken = newRefreshToken
	}

	email, _, _ := auth.GetUserInfo(newAccessToken)
	connectionName := auth.DeriveKiroConnectionName(email, profileArn, region, targetProvider)

	// Dedup by profileArn
	if profileArn != "" {
		existing := findDedupTarget(profileArn, email, "idc")
		if existing != nil {
			existing.AccessToken = newAccessToken
			existing.RefreshToken = refreshToken
			existing.ExpiresAt = expiresAt
			existing.Email = email
			existing.Nickname = connectionName
			if err := config.UpdateAccount(existing.ID, *existing); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			h.pool.Reload()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"account": map[string]interface{}{
					"id":    existing.ID,
					"email": email,
				},
			})
			return
		}
	}

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		Nickname:     connectionName,
		AccessToken:  newAccessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportKiroApiKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ApiKey string `json:"apiKey"`
		Region string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if body.ApiKey == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key is required"})
		return
	}
	region := body.Region
	if region == "" {
		region = "us-east-1"
	}

	profileArn, err := resolveApiKeyProfile(body.ApiKey, region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("API key validation failed: %v", err)})
		return
	}

	email := extractEmailFromJWT(body.ApiKey)

	now := time.Now()
	// ksk_ keys: respect user-selected region. Both us-east-1 and eu-central-1
	// host management./runtime. endpoints; the key's home region is determined
	// at import time, not hardcoded. Default to us-east-1 (Kiro API key primary).
	if strings.HasPrefix(body.ApiKey, "ksk_") {
		if region == "" {
			region = "us-east-1"
		}
	}
	account := config.Account{
		ID:           uuid.New().String(),
		Email:        email,
		AuthMethod:   "api_key",
		Provider:     "Kiro API Key",
		AccessToken:  body.ApiKey,
		RefreshToken: "",
		ProfileArn:   profileArn,
		Region:       region,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ExpiresAt:    now.Add(365 * 24 * 60 * 60).Unix(),
	}
	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Failed to save account: %v", err)})
		return
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]string{
			"id":       account.ID,
			"email":    email,
			"provider": "kiro",
		},
	})
}

// apiImportExternalProvider adds an external OpenAI-compatible or AgentRouter provider as a
// pool account. The provider is optionally validated with a tiny ping request
// before being persisted.
func (h *Handler) apiImportExternalProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL    string `json:"baseUrl"`
		ApiKey     string `json:"apiKey"`
		Name       string `json:"name"`
		Nickname   string `json:"nickname"`
		AuthMethod string `json:"authMethod"`
		Weight     int    `json:"weight"`
		ProxyURL   string `json:"proxyURL"`
		Test       bool   `json:"test"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	baseURL := strings.TrimSpace(body.BaseURL)
	apiKey := strings.TrimSpace(body.ApiKey)
	if baseURL == "" || apiKey == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "baseUrl and apiKey are required"})
		return
	}
	// Normalize: ensure a scheme so the upstream HTTP client doesn't reject.
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(body.Nickname)
	}
	if name == "" {
		// Derive a friendly label from the base URL host.
		if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
			name = u.Host
		} else {
			name = "external-provider"
		}
	}

	authMethod := externalAuthMethod
	providerName := "External OpenAI"
	if strings.ToLower(strings.TrimSpace(body.AuthMethod)) == "agentrouter" {
		authMethod = "agentrouter"
		providerName = "AgentRouter"
	}

	account := config.Account{
		ID:          uuid.New().String(),
		Email:       name,
		Nickname:    name,
		AuthMethod:  authMethod,
		Provider:    providerName,
		AccessToken: apiKey,
		BaseURL:     baseURL,
		Region:      "external",
		Enabled:     true,
		MachineId:   config.GenerateMachineId(),
		Weight:      body.Weight,
		ProxyURL:    strings.TrimSpace(body.ProxyURL),
		// No ExpiresAt — external API keys don't auto-refresh; ensureValidToken
		// treats ExpiresAt==0 as "always valid".
	}

	// Optional live validation: a tiny ping lets the UI confirm the pair works
	// before the operator relies on it. Failure is reported but the account is
	// still saved so the operator can edit it later.
	var testLatencyMs int64
	var testErr string
	if body.Test {
		lat, err := testExternalProvider(&account)
		if err != nil {
			testErr = err.Error()
		} else {
			testLatencyMs = lat.Milliseconds()
		}
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Failed to save provider: %v", err)})
		return
	}
	h.pool.Reload()
	// Fetch & cache the provider's model list so routing picks it correctly.
	go func(acc config.Account) {
		if err := h.fetchAndCacheAccountModels(&acc); err != nil {
			logger.Warnf("[ExternalProvider] Auto model-list fetch failed for %s: %v", acc.Email, err)
		}
	}(account)

	resp := map[string]interface{}{
		"success": true,
		"account": map[string]string{
			"id":       account.ID,
			"email":    account.Email,
			"provider": providerName,
			"baseUrl":  account.BaseURL,
		},
	}
	if body.Test {
		resp["test"] = map[string]interface{}{
			"latencyMs": testLatencyMs,
			"error":     testErr,
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func resolveApiKeyProfile(apiKey, region string) (string, error) {
	if strings.HasPrefix(apiKey, "ksk_") {
		arn, err := resolveKskProfile(apiKey, region)
		if err == nil && arn != "" {
			return arn, nil
		}
		return "", nil
	}
	endpoint := fmt.Sprintf("https://codewhisperer.%s.amazonaws.com", region)
	payload := strings.NewReader(`{"maxResults":10}`)

	req, err := http.NewRequest("POST", endpoint, payload)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("x-amz-target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	client := auth.GetAuthClientForProxy(config.GetProxyURL())
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ListAvailableProfiles returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Profiles []struct {
			Arn        string `json:"arn"`
			ProfileArn string `json:"profileArn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	return "", fmt.Errorf("no profiles found for this API key")
}

func resolveKskProfile(apiKey, region string) (string, error) {
	// ksk_ keys use Smithy protocol: POST to root path with X-Amz-Target header.
	// GetProfile succeeds in the key's home region. Try user-selected region
	// first, then fall back to the two known-working Kiro management regions.
	regions := []string{"us-east-1", "eu-central-1"}
	if region != "" && region != "us-east-1" && region != "eu-central-1" {
		regions = append([]string{region}, regions...)
	} else if region == "eu-central-1" {
		// user explicitly chose eu-central-1 — try it first
		regions = []string{"eu-central-1", "us-east-1"}
	}
	for _, r := range regions {
		endpoint := fmt.Sprintf("https://management.%s.kiro.dev/", r)
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(`{}`))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.GetProfile")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("tokentype", "API_KEY")
		req.Header.Set("Accept", "*/*")
		client := auth.GetAuthClientForProxy(config.GetProxyURL())
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		var result struct {
			Profile struct {
				Arn string `json:"arn"`
			} `json:"profile"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			continue
		}
		if result.Profile.Arn != "" {
			return result.Profile.Arn, nil
		}
	}
	return "", fmt.Errorf("could not resolve profile for ksk_ key")
}

func extractEmailFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	payload = strings.ReplaceAll(payload, "-", "+")
	payload = strings.ReplaceAll(payload, "_", "/")

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}
	if claims.Email != "" {
		return claims.Email
	}
	if claims.PreferredUsername != "" {
		return claims.PreferredUsername
	}
	return claims.Sub
}

func (h *Handler) apiRegisterKiroCli(w http.ResponseWriter, r *http.Request) {
	// Re-register OIDC client for kiro-cli imported account with social refresh fallback.
	creds, err := auth.ImportKiroCli()
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	region := creds.Region
	if region == "" {
		region = "us-east-1"
	}
	tempAccount := &config.Account{
		RefreshToken: creds.RefreshToken,
		AuthMethod:   "social",
		Region:       region,
	}
	newAccessToken, newRefreshToken, expiresAt, profileArn, _, _, err := auth.RefreshAccountToken(tempAccount)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	email, _, _ := auth.GetUserInfo(newAccessToken)
	if profileArn == "" {
		profileArn = creds.ProfileArn
	}

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  newAccessToken,
		RefreshToken: newRefreshToken,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  "kiro-cli-sqlite",
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportSSOCache(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.ImportSSOCache()
	if err != nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	region := "us-east-1"
	if r.URL.Query().Get("region") != "" {
		region = r.URL.Query().Get("region")
	}

	// Register a new OIDC client and refresh the token (same pattern as ImportFromSsoToken).
	accessToken, newRefreshToken, clientID, clientSecret, expiresIn, profileArn, err := auth.ImportFromSsoToken(creds.RefreshToken, region)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	if newRefreshToken != "" {
		creds.RefreshToken = newRefreshToken
	}

	// get user info
	email, _, _ := auth.GetUserInfo(accessToken)

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: creds.RefreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  creds.Source,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// regionFromArn extracts the AWS region from a CodeWhisperer profile ARN of the
// form arn:aws:codewhisperer:<region>:<account>:profile/<id>. Returns "" if the
// ARN is empty or malformed.
func regionFromArn(arn string) string {
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return ""
	}
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// findDedupTarget returns the existing account that a re-login/import should
// update in place, or nil when the credential should be appended as a new
// account. For external_idp (Azure AD) accounts every user in the same AWS org
// shares one Q Developer profile ARN, so profileArn alone is NOT a unique
// identity — require a matching per-user email too, and never dedup when email
// is unresolved (append a fresh account instead of clobbering someone else's).
// All other auth methods keep the original profileArn-only dedup behaviour.
func findDedupTarget(profileArn, email, authMethod string) *config.Account {
	if profileArn == "" {
		return nil
	}
	if authMethod == "external_idp" {
		return config.FindAccountByProfileArnAndEmail(profileArn, email)
	}
	return config.FindAccountByProfileArn(profileArn)
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken   string `json:"accessToken"`
		RefreshToken  string `json:"refreshToken"`
		ClientID      string `json:"clientId"`
		ClientSecret  string `json:"clientSecret"`
		AuthMethod    string `json:"authMethod"`
		Provider      string `json:"provider"`
		Region        string `json:"region"`
		ProfileArn    string `json:"profileArn,omitempty"`
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerURL     string `json:"issuerUrl,omitempty"`
		Scopes        string `json:"scopes,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	// set defaults
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	if req.AuthMethod == "" {
		if req.TokenEndpoint != "" {
			req.AuthMethod = "external_idp"
		} else if req.ClientID != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}
	// normalize authMethod
	switch strings.ToLower(req.AuthMethod) {
	case "external_idp", "externalidp":
		req.AuthMethod = "external_idp"
	case "idc", "builderid":
		req.AuthMethod = "idc"
	case "enterprise":
		// Enterprise SSO is external_idp when an IdP token endpoint is present,
		// otherwise a standard AWS IdC registration.
		if req.TokenEndpoint != "" {
			req.AuthMethod = "external_idp"
		} else {
			req.AuthMethod = "idc"
		}
	case "social", "google", "github":
		req.AuthMethod = "social"
	default:
		if req.TokenEndpoint != "" {
			req.AuthMethod = "external_idp"
		} else if req.ClientID != "" && req.ClientSecret != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}

	// Use refreshToken to get a new accessToken. Import requires a successful refresh first:
	// The locally cached accessToken has no trusted expiry. Guessing a short TTL would cause accounts
	// to always be skipped, preventing background/on-demand refresh (see ensureValidToken & Pick expiry logic).
	tempAccount := &config.Account{
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Region:        req.Region,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        req.Scopes,
	}
	accessToken, newRefreshToken, expiresAt, newProfileArn, _, _, err := auth.RefreshAccountToken(tempAccount)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	if newRefreshToken != "" {
		req.RefreshToken = newRefreshToken
	}

	// Discover the profile ARN explicitly. The OIDC /token refresh used for
	// idc/Builder-ID accounts never returns a profileArn, so newProfileArn is
	// empty here. Persisting an account with an empty ProfileArn makes the
	// upstream CodeWhisperer call reject generateAssistantResponse with
	// HTTP 403 "User is not authorized to make this call". Every other import
	// path resolves the ARN via DiscoverProfileArn — do the same here for parity.
	if newProfileArn == "" && req.AuthMethod != "social" {
		externalIdp := req.AuthMethod == "external_idp"
		// Probe across regions: enterprise IdC profiles often live outside the
		// SSO region (e.g. SSO us-east-1 but profile eu-central-1, served by
		// q.eu-central-1 — codewhisperer.<region> only exists in us-east-1).
		if pa := auth.DiscoverProfileArnMultiRegion(accessToken, req.Region, externalIdp); pa != "" {
			newProfileArn = pa
		}
	}
	// NOTE: do NOT overwrite req.Region with the profile ARN's region. For AWS
	// IdC the SSO/OIDC region (used for token refresh, e.g. us-east-1) often
	// differs from the CodeWhisperer profile region (e.g. eu-central-1). Region
	// must stay the SSO region so future refreshes succeed; the runtime endpoint
	// region is derived from the profile ARN at call time (see regionForAccount).

	// get user info. external_idp access tokens are IdP-issued JWTs (Azure AD),
	// which CodeWhisperer's GetUserInfo rejects, so read the email from the JWT
	// claims instead — same as the SSO-exchange path.
	var email string
	if req.AuthMethod == "external_idp" {
		email = auth.ExtractEmailFromJWT(accessToken)
	} else {
		email, _, _ = auth.GetUserInfo(accessToken)
	}

	// Dedup with the same per-user rule as the SSO/CLI import paths: a re-import
	// of the same identity updates the existing account in place instead of
	// creating a duplicate that a later dedup pass would clobber.
	if existing := findDedupTarget(newProfileArn, email, req.AuthMethod); existing != nil {
		existing.AccessToken = accessToken
		existing.RefreshToken = req.RefreshToken
		existing.ExpiresAt = expiresAt
		existing.Email = email
		existing.ClientID = req.ClientID
		existing.ClientSecret = req.ClientSecret
		existing.AuthMethod = req.AuthMethod
		existing.Region = req.Region
		existing.Enabled = true
		existing.BanStatus = "ACTIVE"
		existing.BanReason = ""
		existing.BanTime = 0
		existing.TokenEndpoint = req.TokenEndpoint
		existing.IssuerURL = req.IssuerURL
		existing.Scopes = req.Scopes
		if err := config.UpdateAccount(existing.ID, *existing); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"account": map[string]interface{}{
				"id":    existing.ID,
				"email": email,
			},
		})
		return
	}

	// create account
	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         email,
		AccessToken:   accessToken,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Provider:      req.Provider,
		Region:        req.Region,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		ProfileArn:    newProfileArn,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        req.Scopes,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	// Aggregate Kiro-side quota (usageCurrent/usageLimit) across all pool accounts.
	// These reflect the real per-account credit quotas reported by Kiro, distinct
	// from totalCredits which only counts credits consumed through OmniProxy.
	var kiroUsageCurrent, kiroUsageLimit, trialUsageCurrent, trialUsageLimit float64
	// Compute totalTokens/totalRequests/totalCredits as the SUM of per-account
	// stats from the pool (same source as /quota/overview and /accounts). This
	// guarantees the top-level stats bar is always consistent with the sum of
	// per-account blocks — no divergence between pages.
	var totalTokens int64
	var totalRequests int64
	var totalCredits float64
	for _, a := range h.pool.GetAllAccountsFull() {
		kiroUsageCurrent += a.UsageCurrent
		kiroUsageLimit += a.UsageLimit
		trialUsageCurrent += a.TrialUsageCurrent
		trialUsageLimit += a.TrialUsageLimit
		totalRequests += int64(a.RequestCount)
		totalTokens += int64(a.TotalTokens)
		totalCredits += a.TotalCredits
	}
	// Available models from cache (same source as /v1/models, minus aliases/combos)
	h.modelsCacheMu.RLock()
	cachedModels := h.cachedModels
	h.modelsCacheMu.RUnlock()
	modelIds := make([]string, 0, len(cachedModels))
	for _, m := range cachedModels {
		if m.ModelId != "" && m.ModelId != "auto" {
			modelIds = append(modelIds, m.ModelId)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":          h.pool.Count(),
		"available":         h.pool.AvailableCount(),
		"totalAccounts":     len(h.pool.GetAllAccountsFull()),
		"totalRequests":     totalRequests,
		"successRequests":   atomic.LoadInt64(&h.successRequests),
		"failedRequests":    atomic.LoadInt64(&h.failedRequests),
		"totalTokens":       totalTokens,
		"totalCredits":      totalCredits,
		"kiroUsageCurrent":  kiroUsageCurrent,
		"kiroUsageLimit":    kiroUsageLimit,
		"trialUsageCurrent": trialUsageCurrent,
		"trialUsageLimit":   trialUsageLimit,
		"availableModels":   len(modelIds),
		"modelIds":          modelIds,
		"uptime":            time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":         config.GetApiKey(),
		"requireApiKey":  config.IsApiKeyRequired(),
		"port":           config.GetPort(),
		"host":           config.GetHost(),
		"allowOverUsage": config.GetAllowOverUsage(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey           *string `json:"apiKey,omitempty"`
		RequireApiKey    *bool   `json:"requireApiKey,omitempty"`
		Password         string  `json:"password,omitempty"`
		AllowOverUsage   *bool   `json:"allowOverUsage,omitempty"`
		CacheDiagnostics *bool   `json:"cacheDiagnostics,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// update overage settings
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}

	// Prompt-cache routing diagnostics. Deliberately runtime-togglable and not
	// pool-affecting: it only changes whether a log line is emitted, so it can
	// be switched on for a measurement window and off again without touching
	// routing behaviour or restarting the proxy.
	if req.CacheDiagnostics != nil {
		if err := config.UpdateCacheDiagnostics(*req.CacheDiagnostics); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	config.ResetAllAccountStats()
	h.pool.ResetStats()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId generates a new machine ID
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiRefreshAccountCredits POST /admin/api/accounts/{id}/credits
// Refreshes the external provider's credit balance via /api/me. Only meaningful
// for external_openai accounts; native Kiro accounts return a friendly error.
func (h *Handler) apiRefreshAccountCredits(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !isExternalAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Credits are only available for external OpenAI-compatible providers"})
		return
	}
	if err := h.refreshExternalCredits(account); err != nil {
		if err == ErrExternalCreditsNotSupported {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "Provider does not expose /api/me — credits API not supported"})
			return
		}
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": "Credit refresh failed: " + err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"credits": map[string]interface{}{
			"creditLimit":      account.ExtCreditLimit,
			"creditsRemaining": account.ExtCreditsRemaining,
			"creditsUsed":      account.ExtCreditsUsed,
			"requestsCount":    account.ExtRequestsCount,
			"tokensUsed":       account.ExtTokensUsed,
			"status":           account.ExtStatus,
			"keyMasked":        account.ExtKeyMasked,
			"lastUsedAt":       account.ExtLastUsedAt,
			"checkedAt":        account.ExtCreditsCheckedAt,
		},
	})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	var req struct {
		Capability string `json:"capability"`
		Model      string `json:"model"`
		Query      string `json:"query"`
		URL        string `json:"url"`
		Prompt     string `json:"prompt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Image tests use the same adapter as the public image endpoint for every
	// account type. This keeps the admin test honest: Kiro accounts return an
	// explicit unsupported response instead of being sent through chat.
	if strings.EqualFold(strings.TrimSpace(req.Capability), "image") {
		start := time.Now()
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			prompt = "A cute orange cat sitting by a sunny window, detailed digital illustration"
		}
		result, err := callImageGeneration(r, account, imageGenerationRequest{Prompt: prompt, Model: req.Model, N: 1})
		if err != nil {
			unsupported := false
			var unsupportedErr *unsupportedCapabilityError
			if errors.As(err, &unsupportedErr) {
				unsupported = true
			}
			w.WriteHeader(serviceErrorStatus(err))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false, "capability": "image", "unsupported": unsupported, "error": err.Error(),
			})
			return
		}
		h.pool.RecordSuccess(account.ID, "image")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "capability": "image", "model": imageModelFor(account, req.Model),
			"reply": fmt.Sprintf("%d image(s) returned", len(result.Data)), "imageCount": len(result.Data),
			"elapsedMs": time.Since(start).Milliseconds(),
		})
		return
	}

	// Service accounts must be tested through their native adapter. In
	// particular, never pass Tavily/Exa/Firecrawl/Jina/OpenRouter credentials
	// through dispatchChat, which would route them to Kiro or OpenAI chat.
	if isServiceAccount(account) {
		capability := strings.ToLower(strings.TrimSpace(req.Capability))
		if capability == "" {
			switch {
			case accountHasCapability(account, "search"):
				capability = "search"
			case accountHasCapability(account, "image"):
				capability = "image"
			}
		}
		if !accountHasCapability(account, capability) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "unsupported service capability"})
			return
		}

		start := time.Now()
		if capability == "search" {
			query := strings.TrimSpace(req.Query)
			if query == "" {
				query = "OmniProxy health check"
			}
			result, err := callSearchProvider(r, account, searchRequest{Query: query, URL: strings.TrimSpace(req.URL), MaxResults: 1})
			if err != nil {
				h.pool.RecordError(account.ID, serviceErrorIsQuota(err), "search")
				w.WriteHeader(serviceErrorStatus(err))
				json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error(), "capability": capability})
				return
			}
			h.pool.RecordSuccess(account.ID, "search")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "capability": capability, "provider": result.Provider,
				"reply": fmt.Sprintf("%d result(s)", len(result.Results)), "resultCount": len(result.Results),
				"elapsedMs": time.Since(start).Milliseconds(),
			})
			return
		}

		// Image requests returned above. A service account without an explicit
		// search capability must not fall through to Kiro chat routing.
		if capability != "search" {
			w.WriteHeader(http.StatusNotImplemented)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false, "capability": capability, "unsupported": true,
				"error": "service account does not support chat testing",
			})
			return
		}
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// AgentRouter's Test Chat is a protocol health check, not a generic
	// account chat. Keep it identical to the documented Anthropic probe so a
	// successful result proves the configured key can reach AgentRouter.
	if isAgentRouterAccount(account) {
		start := time.Now()
		reply, err := CallAgentRouterTest(account)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.RecordSuccess(account.ID, agentRouterTestModel)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"reply":     reply,
			"model":     agentRouterTestModel,
			"elapsedMs": time.Since(start).Milliseconds(),
		})
		return
	}

	// Parse test model from request body (optional)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := dispatchChat(account, kiroPayload, callback)
	if err != nil {
		errMsg := err.Error()
		// Codex accounts: OpenAI returns 401/403 both for a dead session and
		// for a terminated account, so classify on upstream vocabulary before
		// falling through to the Kiro-shaped branches below (which explicitly
		// exclude Codex). Without this, a banned Codex account only ever shows
		// "Log in again" — a button that can never recover it.
		if isCodexAccount(account) {
			switch classifyCodexAuthFailure(err) {
			case codexAuthFailureBanned:
				markCodexAuthFailure(account, err)
				w.WriteHeader(403)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error":     errMsg,
					"banStatus": "BANNED",
					"banned":    true,
					"banReason": account.BanReason,
				})
				return
			case codexAuthFailureReauth:
				markCodexAuthFailure(account, err)
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error":          errMsg,
					"banStatus":      codexReauthRequiredStatus,
					"banned":         false,
					"reauthRequired": true,
					"banReason":      account.BanReason,
				})
				return
			}
		}
		// 403 "temporarily is suspended" → account is banned by AWS.
		// Mark BANNED so the UI reflects the real status instead of showing
		// a raw 500 error on every subsequent test/refresh.
		if isSuspensionErrorMessage(strings.ToLower(errMsg)) && !isExternalAccount(account) && !isCodexAccount(account) {
			account.BanStatus = "BANNED"
			account.BanReason = truncateErrBody([]byte(errMsg))
			account.BanTime = time.Now().Unix()
			account.Enabled = false
			if updateErr := config.UpdateAccountPreservingCredentials(account.ID, *account); updateErr != nil {
				logger.Errorf("[apiTestAccount] Failed to persist BANNED status for %s: %v", account.Email, updateErr)
			} else {
				logger.Warnf("[apiTestAccount] Marked %s as BANNED (suspended): %s", account.Email, errMsg)
			}
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":     errMsg,
				"banStatus": "BANNED",
				"banned":    true,
			})
			return
		}
		// Persistent 403/401 auth error (not suspension) → also ban.
		if isAuthErrorMessage(errMsg) && !isExternalAccount(account) && !isCodexAccount(account) {
			account.BanStatus = "BANNED"
			account.BanReason = "Test failed: " + truncateErrBody([]byte(errMsg))
			account.BanTime = time.Now().Unix()
			account.Enabled = false
			if updateErr := config.UpdateAccountPreservingCredentials(account.ID, *account); updateErr != nil {
				logger.Errorf("[apiTestAccount] Failed to persist BANNED status for %s: %v", account.Email, updateErr)
			} else {
				logger.Warnf("[apiTestAccount] Marked %s as BANNED (auth error): %s", account.Email, errMsg)
			}
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":     errMsg,
				"banStatus": "BANNED",
				"banned":    true,
			})
			return
		}
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	// Test succeeded — if the account was previously banned, clear the ban
	// because the account can actually serve requests. This is the only
	// path that clears a ban: a successful real upstream request proves the
	// account is healthy again.
	wasBanned := account.BanStatus != "" && account.BanStatus != "ACTIVE"
	if wasBanned || !account.Enabled {
		account.BanStatus = "ACTIVE"
		account.BanReason = ""
		account.BanTime = 0
		account.Enabled = true
		if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
			logger.Errorf("[apiTestAccount] Failed to persist ban-clear for %s: %v", account.Email, err)
		} else if wasBanned {
			logger.Infof("[apiTestAccount] Test succeeded — cleared ban for %s", account.Email)
		}
		h.pool.Reload()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"reply":      content,
		"model":      req.Model,
		"banCleared": wasBanned,
	})
}

// apiRefreshAccount refreshes account info (usage, subscription, etc.)
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// The whole refresh body lives in refreshAccountFull so this endpoint and
	// POST /accounts/refresh-all perform identical work per account.
	isService := isServiceAccount(account)
	result, err := h.refreshAccountFull(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	resp := map[string]interface{}{"success": true}
	if result.Info != nil {
		resp["info"] = result.Info
	}
	if result.Message != "" {
		resp["message"] = result.Message
	}
	if result.BanStatus != "" {
		resp["banStatus"] = result.BanStatus
	}
	if isService {
		resp["provider"] = account.Provider
		resp["capabilities"] = account.Capabilities
	}
	json.NewEncoder(w).Encode(resp)
}

// apiGetAccountFull gets full account info (including sensitive fields)
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	// Use GetAllAccountsFull to get ALL accounts with live pool stats overlaid.
	// This ensures banned/disabled accounts also show their correct stats.
	allAccounts := h.pool.GetAllAccountsFull()

	// find specified account
	var account *config.Account
	for i := range allAccounts {
		if allAccounts[i].ID == id {
			account = &allAccounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// return full account info (including sensitive fields)
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"sourceId":          account.SourceID,
		"providerKind":      account.ProviderKind,
		"capabilities":      account.Capabilities,
		"region":            account.Region,
		"baseUrl":           account.BaseURL,
		"imageModel":        account.ImageModel,
		"codexImageModel":   account.CodexImageModel,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      account.RequestCount,
		"errorCount":        account.ErrorCount,
		"totalTokens":       account.TotalTokens,
		"totalCredits":      account.TotalCredits,
		"lastUsed":          account.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels gets available models for an account
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if isServiceAccount(account) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "service accounts do not expose chat models"})
		return
	}

	// External OpenAI-compatible providers expose /v1/models, not Kiro's
	// ListAvailableModels. Calling ListAvailableModels with their access
	// token hits q.external.amazonaws.com (a Kiro/AWS endpoint) and fails
	// with a DNS error. Route them through the dedicated external fetcher.
	if isExternalAccount(account) {
		models, err := fetchExternalProviderModels(account)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(id, modelIDs)
		h.modelsCacheMu.Lock()
		h.cachedModels = mergeUniqueModels(h.cachedModels, models)
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"models":  models,
		})
		return
	}

	// Codex (ChatGPT subscription) accounts expose a fixed set of
	// subscription-tier models — no Kiro ListAvailableModels endpoint to
	// call. fetchAndCacheAccountModels seeds the canonical Codex model
	// list into the routing cache and returns it via the cached path.
	if isCodexAccount(account) {
		if err := h.fetchAndCacheAccountModels(account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		cached := h.pool.GetModelList(id)
		models := make([]ModelInfo, 0, len(cached))
		for _, mid := range cached {
			models = append(models, ModelInfo{ModelId: mid, ModelName: mid})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"models":  models,
		})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Synchronously update routing cache
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached returns cached model list for an account (no live fetch)
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

func (h *Handler) apiGetAccountImageModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	models := make([]map[string]interface{}, 0)
	source := "kiro"
	supported := false
	reason := "upstream does not expose image generation"
	appendModel := func(id, name, modelSource string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		for _, existing := range models {
			if existing["id"] == id {
				return
			}
		}
		models = append(models, map[string]interface{}{"id": id, "name": firstNonEmpty(name, id), "source": modelSource})
	}

	switch {
	case isCodexAccount(account):
		// Codex image generation runs through the image_generation tool, whose
		// "model" field only accepts the gpt-image-* family. Listing the text
		// models (gpt-5.6-*) here was wrong: selecting one produced an
		// upstream rejection. Offer the configured model first (if it is a
		// valid image model), then the known gpt-image-* catalog.
		source, supported, reason = "codex", true, ""
		if configured := strings.TrimSpace(account.CodexImageModel); isCodexImageToolModel(configured) {
			appendModel(configured, configured, source)
		}
		for _, model := range codexImageToolModels() {
			appendModel(model.ID, model.Name, source)
		}
	case isExternalAccount(account):
		source = "external"
		discovered, err := fetchExternalProviderModels(account)
		if err == nil {
			for _, model := range discovered {
				// OpenAI-compatible catalogs often omit output modalities. Keep
				// those IDs selectable as candidates; the real image request is
				// still the authority on whether the upstream accepts the model.
				verified := modelSupportsImageOutput(model)
				modelSource := source
				if !verified {
					modelSource = source + ":candidate"
				}
				appendImageModel(&models, model.ModelId, model.ModelName, modelSource, verified)
			}
		}
		if err == nil && len(models) > 0 {
			supported = true
			reason = ""
		} else if err != nil {
			reason = "model discovery failed; custom model is still allowed"
		} else {
			reason = "upstream returned no models; custom model is still allowed"
		}
	case accountHasCapability(account, "image"):
		source, supported, reason = "service", true, ""
		appendModel(imageModelFor(account, ""), imageModelFor(account, ""), source)
		appendModel("openai/gpt-5-image", "openai/gpt-5-image", source)
	default:
		// Kiro's model catalog is useful for selecting a test model even
		// though Kiro currently has no native image-generation route.
		source = "kiro:candidate"
		for _, modelID := range h.pool.GetModelList(account.ID) {
			appendModel(modelID, modelID, source)
		}
		if len(models) == 0 {
			if discovered, err := ListAvailableModels(account); err == nil {
				for _, model := range discovered {
					appendModel(model.ModelId, model.ModelName, source)
				}
			}
		}
		if len(models) > 0 {
			reason = "Kiro catalog models are selectable, but this account does not expose native image generation"
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"models":        models,
		"source":        source,
		"supported":     supported,
		"customAllowed": true,
		"reason":        reason,
	})
}

func appendImageModel(models *[]map[string]interface{}, id, name, source string, verified bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	for _, existing := range *models {
		if existing["id"] == id {
			return
		}
	}
	*models = append(*models, map[string]interface{}{
		"id": id, "name": firstNonEmpty(name, id), "source": source, "verified": verified,
	})
}

// ==================== Static file serving ====================

// setNoCacheHeaders ensures the browser always fetches the latest version
// of admin web assets (HTML/JS/CSS). Without this, browsers cache
// usage.js / index.html aggressively and serve stale code after a server
// upgrade, which causes tabs to show empty content or call endpoints
// that no longer exist.
func setNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	setNoCacheHeaders(w)
	http.ServeFile(w, r, filepath.Join(h.webDir, "index.html"))
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	setNoCacheHeaders(w)
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, filepath.Join(h.webDir, path))
}

// apiGetThinkingConfig gets the thinking config
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig updates the thinking config
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// validate format
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig gets the endpoint config
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig updates the endpoint config
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig applies proxy config to all outbound HTTP clients (Kiro API + auth module)
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy gets the current proxy config
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy updates proxy config and applies immediately
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// validate proxy URL format (when non-empty)
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// immediately apply new proxy config
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion gets version info
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts exports account credentials
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // empty exports all
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// if body is empty or parse fails, export all
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// if IDs specified, only export those
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// build export format compatible with Kiro Account Manager
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ProfileArn   string `json:"profileArn,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// map provider to idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// map authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// map subscription type
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ProfileArn:   a.ProfileArn,
				ExpiresAt:    a.ExpiresAt * 1000, // convert to millisecond timestamp
				AuthMethod:   authMethod,
				Provider:     a.Provider,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func (h *Handler) apiShutdown(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "shutting down"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	selfSignalInterrupt()
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// apiGetUsageStats returns full usage statistics for the usage page.
func (h *Handler) apiGetUsageStats(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "24h"
	}
	stats := h.usageTracker.GetStats(period)
	json.NewEncoder(w).Encode(stats)
}

// apiGetUsageChart returns time-bucketed chart data.
func (h *Handler) apiGetUsageChart(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "7d"
	}
	data := h.usageTracker.GetChartData(period)
	json.NewEncoder(w).Encode(data)
}

// apiUsageStream provides SSE streaming for real-time usage updates.
func (h *Handler) apiUsageStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	listener := h.usageTracker.SubscribeSSE()
	defer h.usageTracker.UnsubscribeSSE(listener)

	// Send initial stats immediately
	stats := h.usageTracker.GetStats("24h")
	if data, err := json.Marshal(stats); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	notify := r.Context().Done()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-notify:
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case data, ok := <-listener.ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// apiGetLogs returns the current contents of the in-memory log ring buffer.
func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"lines": logger.LogBuf.Lines(),
	})
}

// apiLogsStream live-tails the logger via SSE. It subscribes to logger.Subscribe
// (which invokes the callback synchronously from the logging goroutine), pushing
// each line into a buffered channel so the logging path never blocks; lines are
// dropped for this client if it falls behind rather than stalling the logger.
func (h *Handler) apiLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Resume cursor. Browsers send Last-Event-ID automatically on their own
	// reconnect; a client that constructs a fresh EventSource has to pass
	// ?lastEventId= explicitly because the header is not settable there.
	var after uint64
	resume := r.Header.Get("Last-Event-ID")
	if resume == "" {
		resume = r.URL.Query().Get("lastEventId")
	}
	if resume != "" {
		if v, err := strconv.ParseUint(resume, 10, 64); err == nil {
			after = v
		}
	}

	// tail caps the initial replay. Without it every reconnect re-sent the whole
	// 2048-line buffer (~480 KB), most of which the viewer immediately discarded
	// because it only retains a few hundred lines.
	tail := 0
	if t := r.URL.Query().Get("tail"); t != "" {
		if v, err := strconv.Atoi(t); err == nil && v > 0 {
			tail = v
		}
	}
	if after == 0 && tail == 0 {
		tail = 500 // matches the viewer's default retention
	}

	// Buffered so a slow client never blocks the logging goroutine.
	type seqLine struct {
		seq  uint64
		line string
	}
	lineCh := make(chan seqLine, 256)
	// Subscribe BEFORE taking the snapshot so no line can slip through the gap
	// between the two; overlap is removed by the seq > lastSent check below.
	unsub := logger.SubscribeSeq(func(seq uint64, line string) {
		select {
		case lineCh <- seqLine{seq: seq, line: line}:
		default: // drop for this client when its buffer is full
		}
	})
	defer unsub()

	writeLine := func(seq uint64, line string) {
		if data, err := json.Marshal(map[string]string{"line": line}); err == nil {
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, data)
		}
	}

	// Initial replay: only what the client is missing.
	var lastSent uint64
	for _, sl := range logger.LogBuf.SeqLines(after, tail) {
		writeLine(sl.Seq, sl.Line)
		lastSent = sl.Seq
	}
	flusher.Flush()

	notify := r.Context().Done()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-notify:
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case sl := <-lineCh:
			if sl.seq <= lastSent {
				continue // already delivered in the snapshot
			}
			writeLine(sl.seq, sl.line)
			lastSent = sl.seq
			flusher.Flush()
		}
	}
}

// apiGetUsageRequestDetails returns paginated request details.
func (h *Handler) apiGetUsageRequestDetails(w http.ResponseWriter, r *http.Request) {
	page := 1
	pageSize := 20
	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := parseInt(p); err == nil && v > 0 {
			page = v
		}
	}
	if ps := r.URL.Query().Get("pageSize"); ps != "" {
		if v, err := parseInt(ps); err == nil && v > 0 && v <= 100 {
			pageSize = v
		}
	}

	stats := h.usageTracker.GetStats("all")
	allRecs := stats.RecentRequests

	// Apply filters
	providerFilter := r.URL.Query().Get("provider")
	startDate := r.URL.Query().Get("startDate")
	endDate := r.URL.Query().Get("endDate")

	var filtered []RequestRecord
	for _, rec := range allRecs {
		if providerFilter != "" && rec.Provider != providerFilter {
			continue
		}
		if startDate != "" && rec.Timestamp < startDate {
			continue
		}
		if endDate != "" && rec.Timestamp > endDate+"T23:59:59Z" {
			continue
		}
		filtered = append(filtered, rec)
	}

	totalItems := len(filtered)
	totalPages := (totalItems + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	if end > len(filtered) {
		end = len(filtered)
	}
	pageData := filtered[start:end]

	// Convert to detail format
	// AccountName and Error travel with each detail row so the UI can attribute
	// a failure to a specific account without a second lookup. The account
	// label is non-secret metadata (nickname/email/short ID) produced by
	// resolveAccountMeta; credentials are never included.
	type DetailItem struct {
		Timestamp   string         `json:"timestamp"`
		Model       string         `json:"model"`
		Provider    string         `json:"provider"`
		AccountID   string         `json:"accountId"`
		AccountName string         `json:"accountName,omitempty"`
		Status      string         `json:"status"`
		Error       string         `json:"error,omitempty"`
		Tokens      map[string]int `json:"tokens"`
		Latency     map[string]int `json:"latency"`
	}
	details := make([]DetailItem, 0, len(pageData))
	for _, rec := range pageData {
		accountName := rec.AccountName
		if accountName == "" && rec.AccountID != "" {
			if _, resolved := resolveAccountMeta(rec.AccountID); resolved != "" {
				accountName = resolved
			}
		}
		details = append(details, DetailItem{
			Timestamp:   rec.Timestamp,
			Model:       rec.Model,
			Provider:    rec.Provider,
			AccountID:   rec.AccountID,
			AccountName: accountName,
			Status:      rec.Status,
			Error:       rec.Error,
			Tokens: map[string]int{
				"prompt_tokens":     rec.InputTokens,
				"completion_tokens": rec.OutputTokens,
			},
			Latency: map[string]int{},
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"details":    details,
		"pagination": map[string]int{"page": page, "pageSize": pageSize, "totalItems": totalItems, "totalPages": totalPages},
	})
}

// apiGetUsageProviders returns list of unique providers from usage data.
func (h *Handler) apiGetUsageProviders(w http.ResponseWriter, r *http.Request) {
	stats := h.usageTracker.GetStats("all")

	providerSet := make(map[string]bool)
	for _, rec := range stats.RecentRequests {
		if rec.Provider != "" {
			providerSet[rec.Provider] = true
		}
	}

	type ProviderInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	providers := make([]ProviderInfo, 0, len(providerSet))
	for p := range providerSet {
		providers = append(providers, ProviderInfo{ID: p, Name: p})
	}
	// Sort
	for i := 0; i < len(providers); i++ {
		for j := i + 1; j < len(providers); j++ {
			if providers[i].Name > providers[j].Name {
				providers[i], providers[j] = providers[j], providers[i]
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers": providers,
	})
}

// apiGetPricing GET /admin/api/pricing
// Returns the model pricing table used to compute RealCost in usage stats.
// The UI uses this to show $/1M-token rates alongside the effective-tokens
// column in the Usage by Model table, and to explain how RealCost was
// derived. Includes both built-in pricing and any custom overrides.
func (h *Handler) apiGetPricing(w http.ResponseWriter, r *http.Request) {
	// Build a combined map: built-in table + custom overrides (custom wins).
	combined := make(map[string]ModelPricing, len(pricingTable)+8)
	for model, p := range pricingTable {
		combined[model] = p
	}
	customPricing.Range(func(k, v interface{}) bool {
		if model, ok := k.(string); ok {
			if p, ok := v.(ModelPricing); ok {
				combined[model] = p
			}
		}
		return true
	})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pricing": combined,
		"sources": map[string]string{
			"openai":    "https://developers.openai.com/api/docs/pricing",
			"anthropic": "https://platform.claude.com/docs/en/about-claude/pricing",
		},
		"note": "Prices are USD per 1M tokens, sourced from official provider pricing pages. RealCost = (input-cached)*InputPerM + cached*CachedPerM + output*OutputPerM, divided by 1M.",
	})
}

func parseInt(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}
