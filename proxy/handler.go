package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"superkiro/auth"
	"superkiro/config"
	"superkiro/logger"
	"superkiro/pool"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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


// isSuperKiroActiveProvider checks whether the ACTIVE (uncommented)
// model_provider in a TOML config file is set to "superkiro".
// A line starting with # is a comment and is ignored.
func isSuperKiroActiveProvider(data []byte) bool {
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
		if strings.Contains(trimmed, `"superkiro"`) {
			return true
		}
	}
	return false
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
		if isSuperKiroActiveProvider(data) {
			continue
		}
		backupPath := fmt.Sprintf("%s.superkiro.bak.%d", p, time.Now().Unix())
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
	HasSuperKiro bool `json:"hasSuperKiro"`
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

func checkToolHasSuperKiro(toolID string) bool {
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
		if strings.HasPrefix(model, "superkiro/") {
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
		return strings.Contains(string(data), "model_provider = \"superkiro\"")

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
			strings.Contains(strings.ToLower(string(data)), "superkiro")

	case "hermes":
		path := filepath.Join(homeDir, ".hermes", "config.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		configExists = true
		return strings.Contains(string(data), "base_url:")

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
		return strings.Contains(string(data), "\"9router\"")

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
			if title, _ := e["title"].(string); strings.EqualFold(title, "SuperKiro") {
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
			HasSuperKiro: checkToolHasSuperKiro(id),
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
		usageTracker:     GetUsageTracker(),
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

		// check if token needs refresh
		if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
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

		// refresh account info (skip for external IdP — their tokens cannot call CodeWhisperer APIs)
		if !(account.AuthMethod == "external_idp") && (account.BanStatus == "" || account.BanStatus == "ACTIVE") {
			info, err := RefreshAccountInfo(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
				continue
			}
			config.UpdateAccountInfo(account.ID, *info)
			logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
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
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels returns model list
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// try using cached real model list
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.refreshModelsCache()
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// add alias models
	models = append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)

	// Append combo entries so agents can discover named combos in /v1/models
	for _, combo := range config.ListCombos() {
		models = append(models, buildModelInfo(combo.Name, "combo", true))
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			models = append(models, buildModelInfo(m.ModelId, "anthropic", supportsImage))
			// auto-generate thinking variants
			models = append(models, buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage))
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
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
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
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
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

	// Check if model is a combo name FIRST, before thinking/alias resolution.
	// This prevents alias mappings (e.g. "gpt-4o" → "claude-sonnet-4.5") from
	// defeating combo detection when a combo shares an alias name.
	if comboName, comboModels, ok := resolveComboModels(req.Model); ok {
		body, _ := json.Marshal(req)
		h.handleComboRequest(w, r, comboName, comboModels, body, "claude")
		return
	}

	// parse model and thinking mode
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// transform request
	kiroPayload := ClaudeToKiro(&req, thinking)

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
	messageStarted := false
	var messageStartUsage promptCacheUsage

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
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	for attempt := 0; ; attempt++ {
		logger.Warnf("[CLAUDE-STREAM] model=%s attempt=%d pool_accounts=%d excluded=%v",
			model, attempt, h.pool.Count(), excluded)
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			logger.Warnf("[CLAUDE-STREAM] model=%s no account found after %d attempts",
				model, attempt)
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
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
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
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
							sendText(string(runes[:safeLen]), 0)
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
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
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
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
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
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		h.usageTracker.TrackActive(account.ID, "claude", model)
		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			//  try refresh+retry before rotating accounts
			if h.tryRefreshAndRetry(account, payload, callback, err) {
				h.pool.RecordSuccess(account.ID, model)
				// Retry succeeded, proceed to success path
				goto skipAccountHandling
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

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		h.recordUsage(apiKeyID, account.ID, model, "claude", inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)

		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}

		ensureMessageStart()
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordFailure()
	h.sendClaudeError(w, 500, "api_error", lastErr.Error())
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
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

// recordUsage records a successful request to the usage tracker.
// Must be called after recordSuccessForApiKey with the same token/cost values.
func (h *Handler) recordUsage(apiKeyID, accountID, model, endpoint string, inputTokens, outputTokens int, credits float64) {
	if h.usageTracker == nil {
		return
	}
	provider := "unknown"
	if accountID != "" {
		// Try to find provider from account info
		for _, a := range config.GetAccounts() {
			if a.ID == accountID {
				if a.Provider != "" {
					provider = a.Provider
				}
				break
			}
		}
	}
	accountName := ""
	if accountID != "" {
		for _, a := range config.GetAccounts() {
			if a.ID == accountID {
				if a.Nickname != "" {
					accountName = a.Nickname
				} else if a.Email != "" {
					accountName = a.Email
				} else {
					accountName = accountID[:8]
				}
				break
			}
		}
	}
	rec := RequestRecord{
		Model:        model,
		Provider:     provider,
		AccountID:    accountID,
		AccountName:  accountName,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Cost:         credits,
		Status:       "success",
		Endpoint:     endpoint,
		APIKeyID:     apiKeyID,
	}
	h.usageTracker.Append(rec)
}

// handleClaudeNonStream handles Claude non-streaming response
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; ; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			h.usageTracker.RemoveActive(account.ID)
			excluded[account.ID] = true
			h.handleAccountFailure(account, err, model)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
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
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		h.usageTracker.TrackActive(account.ID, "claude", model)
		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			//  try refresh+retry before rotating accounts
			if h.tryRefreshAndRetry(account, payload, callback, err) {
				goto skipNonStreamHandling
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

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordUsage(apiKeyID, account.ID, model, "claude", inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID, model)
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
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
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

	h.recordFailure()
	h.sendClaudeError(w, 500, "api_error", lastErr.Error())
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

	// Check if model is a combo name FIRST, before thinking/alias resolution.
	if comboName, comboModels, ok := resolveComboModels(req.Model); ok {
		body, _ := json.Marshal(req)
		h.handleComboRequest(w, r, comboName, comboModels, body, "openai")
		return
	}

	// parse model and thinking mode
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

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

	for attempt := 0; ; attempt++ {
		logger.Warnf("[OPENAI-STREAM] model=%s attempt=%d pool_accounts=%d excluded=%v",
			model, attempt, h.pool.Count(), excluded)
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			logger.Warnf("[OPENAI-STREAM] model=%s no account found after %d attempts",
				model, attempt)
			break
		}
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
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
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
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		h.usageTracker.TrackActive(account.ID, "openai", model)
		err := CallKiroAPI(account, payload, callback)
			if err != nil {
				lastErr = err
				//  try refresh+retry before rotating accounts
				if h.tryRefreshAndRetry(account, payload, callback, err) {
					h.pool.RecordSuccess(account.ID, model)
					goto skipOpenAIStreamHandling
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

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordUsage(apiKeyID, account.ID, model, "claude", inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
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
			"usage": map[string]int{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailure()
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
}

// handleOpenAINonStream handles OpenAI non-streaming response
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; ; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
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

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		h.usageTracker.TrackActive(account.ID, "openai", model)
		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			// try refresh+retry before rotating accounts
			if h.tryRefreshAndRetry(account, payload, callback, err) {
				goto skipOpenAINonStreamHandling
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

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordUsage(apiKeyID, account.ID, model, "claude", inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailure()
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
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
	if !pool.IsAuthFailure(err) {
		return false
	}
	logger.Warnf("[AuthRetry] Auth failure for %s, attempting token refresh + retry", account.Email)
	newAccessToken, newRefreshToken, newExpiresAt, profileArn, _, _, refreshErr := auth.RefreshAccountToken(account)
	if refreshErr != nil || newAccessToken == "" {
		logger.Warnf("[AuthRetry] Token refresh failed for %s: %v", account.Email, refreshErr)
		return false
	}
	account.AccessToken = newAccessToken
	if newRefreshToken != "" {
		account.RefreshToken = newRefreshToken
	}
	account.ExpiresAt = newExpiresAt
	h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}
	retryErr := CallKiroAPI(account, payload, callback)
	if retryErr != nil {
		logger.Warnf("[AuthRetry] Retry after refresh failed for %s: %v", account.Email, retryErr)
		return false
	}
	logger.Infof("[AuthRetry] Token refresh + retry succeeded for %s", account.Email)
	return true
}

// ensureValidToken ensures token is valid
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
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
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
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
			if account.ExpiresAt > 0 && time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
				return nil
			}
		}
		h.handleAccountFailure(account, err, "")
		return err
	}

	// Record rotation so siblings don't hit upstream with stale tokens.
	auth.RecordRotation(account.RefreshToken, accessToken, refreshToken, profileArn, expiresAt)

	// update memory
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// persist
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
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
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh must match before generic /refresh to avoid interception
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
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
		h.apiGetUsageStats(w, r)
	case path == "/usage/chart" && r.Method == "GET":
		h.apiGetUsageChart(w, r)
	case path == "/usage/stream" && r.Method == "GET":
		h.apiUsageStream(w, r)
	case path == "/usage/request-details" && r.Method == "GET":
		h.apiGetUsageRequestDetails(w, r)
	case path == "/usage/providers" && r.Method == "GET":
		h.apiGetUsageProviders(w, r)
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

// CLI tools
func (h *Handler) apiApplyCliToolSettings(w http.ResponseWriter, r *http.Request, toolID string) {
	var req struct {
		BaseURL       string            `json:"baseUrl"`
		APIKey        string            `json:"apiKey"`
		Model         string            `json:"model"`
		Models        []string          `json:"models"`
		ActiveModel   string            `json:"activeModel"`
		SubagentModel string            `json:"subagentModel"`
		Env           map[string]string `json:"env"`
		AgentModels   map[string]string `json:"agentModels"`
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
			for k, v := range req.Env {
				if v != "" {
					if k == "ANTHROPIC_BASE_URL" {
						v = stripV1(v)
					}
					env[k] = v
				}
			}
		}
		current["hasCompletedOnboarding"] = true
		current["env"] = env
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
		current["provider"] = map[string]interface{}{"superkiro": provider}
		if current["model"] != nil {
			delete(current, "model")
		}
		if activeM != "" {
			current["model"] = "superkiro/" + activeM
		}
		if subM != "" {
			current["agent"] = map[string]interface{}{
				"explorer": map[string]interface{}{
					"description": "Fast explorer subagent for codebase exploration",
					"mode":        "subagent",
					"model":       "superkiro/" + subM,
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
			"actModeApiProvider":     "openai",
			"planModeApiProvider":    "openai",
			"openAiBaseUrl":          baseURL,
			"openAiModelId":          model,
			"planModeOpenAiModelId":  model,
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
		if err := MergeCodexConfig(homeDir, model, bpURL, subagent); err != nil {
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
			hermesModel = "provider/model-id"
		}
		yamlContent := fmt.Sprintf(`model:
  default: "%s"
  provider: "custom"
  base_url: "%s"
`, hermesModel, hermesBPURL)
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
				"model":             m,
				"id":                fmt.Sprintf("custom:9Router-%d", i),
				"index":             i,
				"baseUrl":           droidBPURL,
				"apiKey":            req.APIKey,
				"displayName":       m,
				"maxOutputTokens":   131072,
				"noImageSupport":    false,
				"provider":          "openai",
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
			ocModel = "provider/model-id"
		}
		currentOC := map[string]interface{}{}
		if data, err := os.ReadFile(filepath.Join(ocDir, "openclaw.json")); err == nil {
			json.Unmarshal(data, &currentOC)
		}
		currentOC["models"] = map[string]interface{}{
			"providers": map[string]interface{}{
				"9router": map[string]interface{}{
					"baseUrl": ocBPURL,
					"apiKey":  req.APIKey,
					"api":     "openai-completions",
					"models": []map[string]string{
						{"id": ocModel, "name": ocModel},
					},
				},
			},
		}
		agentModels := map[string]string{}
		if req.AgentModels != nil {
			agentModels = req.AgentModels
		}
		agentsList := []map[string]interface{}{
			{"id": "default", "model": "9router/" + ocModel, "primary": true},
		}
		for agID, agModel := range agentModels {
			agentsList = append(agentsList, map[string]interface{}{
				"id":    agID,
				"model": "9router/" + agModel,
			})
		}
		currentOC["agents"] = map[string]interface{}{
			"defaults": map[string]interface{}{
				"model": map[string]string{"primary": "9router/" + ocModel},
				"models": map[string]interface{}{
					"9router/" + ocModel: map[string]interface{}{},
				},
			},
			"list": agentsList,
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
			if p, _ := providers["superkiro"].(map[string]interface{}); p != nil {
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
					subagentModel = strings.TrimPrefix(m, "superkiro/")
				}
			}
		}
		return &CliToolSettings{
			BaseURL:       baseUrl,
			APIKey:        apiKey,
			Models:        models,
			ActiveModel:   strings.TrimPrefix(activeModel, "superkiro/"),
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
		apiKey, _ := env["ANTHROPIC_AUTH_TOKEN"].(string)
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
		agentModels = map[string]string{}
		if agents, _ := cfg["agents"].(map[string]interface{}); agents != nil {
			if list, _ := agents["list"].([]interface{}); list != nil {
				for _, a := range list {
					if am, _ := a.(map[string]interface{}); am != nil {
						if id, _ := am["id"].(string); id != "" {
							if m, _ := am["model"].(string); m != "" {
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
			if !strings.EqualFold(title, "SuperKiro") {
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
	Running  bool              `json:"running"`
	Cert     bool              `json:"cert"`
	DNS      map[string]bool   `json:"dns"`
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
		APIKey      string `json:"apiKey"`
		SudoPass    string `json:"sudoPassword"`
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
		Tool        string `json:"tool"`
		Action      string `json:"action"` // "enable" or "disable"
		SudoPass    string `json:"sudoPassword"`
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
	mitmDir := filepath.Join(homeDir, ".superkiro", "mitm")
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
			Installed bool                   `json:"installed"`
			Models    []map[string]interface{} `json:"models"`
			Has9Router bool                  `json:"has9Router"`
		}
		if data, err := os.ReadFile(modelsPath); err == nil {
			var models []map[string]interface{}
			if json.Unmarshal(data, &models) == nil {
				cfg.Models = models
				for _, m := range models {
					if title, _ := m["title"].(string); strings.Contains(strings.ToLower(title), "superkiro") {
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
			if strings.Contains(strings.ToLower(title), "superkiro") {
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
				"title":         "SuperKiro",
				"provider":      "openai",
				"model":         m,
				"apiKey":        req.APIKey,
				"baseUrl":       req.BaseURL,
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
			if strings.Contains(strings.ToLower(title), "superkiro") {
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
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// merge runtime stats
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// hide sensitive info
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// get runtime stats
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
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

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
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
				config.UpdateAccount(a.ID, a)
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
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
			ProfileArn   string `json:"profileArn,omitempty"`
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
		Region string `json:"region"`
			ProfileArn   string `json:"profileArn,omitempty"`
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
			ProfileArn   string `json:"profileArn,omitempty"`
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
		existing := config.FindAccountByProfileArn(profileArn)
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
		existing := config.FindAccountByProfileArn(profileArn)
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
		existing := config.FindAccountByProfileArn(profileArn)
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
		existing := config.FindAccountByProfileArn(profileArn)
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
	account := config.Account{
		ID:           uuid.New().String(),
		Email:        email,
		AuthMethod:   "api_key",
		Provider:     "API Key",
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

func resolveApiKeyProfile(apiKey, region string) (string, error) {
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

	for _, p := range result.Profiles {
			arn := p.Arn
			if arn == "" {
				arn = p.ProfileArn
			}
			if arn != "" {
				parts := strings.Split(arn, ":")
				if len(parts) >= 5 && parts[3] == region {
					return arn, nil
				}
			}
		}
	for _, p := range result.Profiles {
		arn := p.Arn
		if arn == "" {
			arn = p.ProfileArn
		}
		if arn != "" {
			return arn, nil
		}
	}

	return "", fmt.Errorf("no profiles found for this API key")
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

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		AuthMethod   string `json:"authMethod"`
		Provider     string `json:"provider"`
		Region       string `json:"region"`
			ProfileArn   string `json:"profileArn,omitempty"`
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
		if req.ClientID != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}
	// normalize authMethod
	switch strings.ToLower(req.AuthMethod) {
	case "idc", "builderid", "enterprise":
		req.AuthMethod = "idc"
	case "social", "google", "github":
		req.AuthMethod = "social"
	default:
		if req.ClientID != "" && req.ClientSecret != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}

	// Use refreshToken to get a new accessToken. Import requires a successful refresh first:
	// The locally cached accessToken has no trusted expiry. Guessing a short TTL would cause accounts
	// to always be skipped, preventing background/on-demand refresh (see ensureValidToken & Pick expiry logic).
	tempAccount := &config.Account{
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Region:       req.Region,
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

	// get user info
	email, _, _ := auth.GetUserInfo(accessToken)

	// create account
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Provider:     req.Provider,
		Region:       req.Region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   newProfileArn,
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
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests,
		"successRequests": h.successRequests,
		"failedRequests":  h.failedRequests,
		"totalTokens":     h.totalTokens,
		"totalCredits":    h.totalCredits,
		"uptime":          time.Now().Unix() - h.startTime,
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
		ApiKey         *string `json:"apiKey,omitempty"`
		RequireApiKey  *bool   `json:"requireApiKey,omitempty"`
		Password       string  `json:"password,omitempty"`
		AllowOverUsage *bool   `json:"allowOverUsage,omitempty"`
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
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId generates a new machine ID
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
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

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
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

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
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

	// First try to refresh the token (regardless of expiry, to ensure token is valid)
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, _, _, err := auth.RefreshAccountToken(account)
		if err != nil {
			return err
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(id, profileArn)
		}
		return nil
	}

	// check if token is expiring soon, refresh first
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// get account info
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// check if ban-related error
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// ban status already handled in RefreshAccountInfo, silently return success
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// if 403/401, token is invalid, try refresh then retry
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// retry
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// Still failed after retry, check if account is banned
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// only show error for other errors
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// save to config
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull gets full account info (including sensitive fields)
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// find specified account
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

	// get runtime stats
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
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
		"region":            account.Region,
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
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
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

// ==================== Static file serving ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(h.webDir, "index.html"))
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
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
	type DetailItem struct {
		Timestamp  string `json:"timestamp"`
		Model      string `json:"model"`
		Provider   string `json:"provider"`
		AccountID  string `json:"accountId"`
		Status     string `json:"status"`
		Tokens     map[string]int `json:"tokens"`
		Latency    map[string]int `json:"latency"`
	}
	details := make([]DetailItem, 0, len(pageData))
	for _, rec := range pageData {
		details = append(details, DetailItem{
			Timestamp: rec.Timestamp,
			Model:     rec.Model,
			Provider:  rec.Provider,
			AccountID: rec.AccountID,
			Status:    rec.Status,
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

func parseInt(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}
