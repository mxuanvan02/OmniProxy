// codex_registry_discovery.go — Dynamic model discovery from OpenAI's Codex
// backend registry. The upstream /backend-api/codex/models endpoint returns
// the live model catalog gated by client_version (the upstream uses this
// to roll out new models to newer CLI versions first). OmniProxy queries
// this endpoint during refreshModelsCache to stay current with OpenAI's
// model releases instead of relying solely on the hardcoded static list.
//
// Source: verified live against https://chatgpt.com/backend-api/codex/models
// with client_version=0.153.4 (2026-09-07). The endpoint requires
// client_version query parameter (400 without it) and returns different
// model sets depending on the version:
//   0.142.5 -> 3 models (gpt-5.5, gpt-5.4-mini, codex-auto-review)
//   0.153.4 -> 8 models (adds gpt-6-astra, gpt-5.6-sol/terra/luna, gpt-reserve)
//
// The static list in codexSubscriptionModels() serves as fallback when the
// registry is unreachable, ensuring the proxy always has a routable model
// set even if OpenAI's backend is down.
package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"omniproxy/auth"
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"time"
)

// codexRegistryClientVersion is the client_version parameter sent to the
// upstream /backend-api/codex/models endpoint. The upstream gates model
// visibility by this value: older versions see fewer models. Set this to
// the current Codex CLI stable release to get the full model set.
// Updated: 2026-09-07 — codex-cli latest is 0.153.4.
const codexRegistryClientVersion = "0.153.4"

// codexRegistryEntry is one model from the upstream /backend-api/codex/models
// response. The upstream returns ~40 fields per model; we only parse what
// the routing and catalog layers need.
type codexRegistryEntry struct {
	Slug                     string `json:"slug"`
	DisplayName              string `json:"display_name"`
	Description              string `json:"description"`
	ContextWindow            int    `json:"context_window"`
	Visibility               string `json:"visibility"`
	SupportedReasoningLevels []struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	} `json:"supported_reasoning_levels"`
}

// codexRegistryResponse is the top-level JSON shape returned by the upstream.
type codexRegistryResponse struct {
	Models []codexRegistryEntry `json:"models"`
}

// fetchCodexRegistryModels queries the upstream /backend-api/codex/models
// endpoint and converts the response into ModelInfo entries suitable for
// the routing cache. Falls back to codexSubscriptionModels() on any error
// (network, auth, parse) so the proxy always has a usable model set.
//
// The fallback is logged at Warn level so the operator knows discovery
// failed and can investigate. A registry failure is not fatal — the static
// list contains all models the proxy was built to support.
//
// The account parameter provides the access token and chatgpt-account-id
// needed for upstream auth. If either is missing, the function falls back
// immediately without making a network call.
func fetchCodexRegistryModels(account *config.Account) []ModelInfo {
	if account == nil || !isCodexAccount(account) {
		return codexSubscriptionModels()
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		logger.Warnf("[CodexRegistry] %s has no access token, using static model list", account.Email)
		return codexSubscriptionModels()
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		accountID = auth.ExtractCodexAccountIDPublic(accessToken)
		if accountID == "" {
			logger.Warnf("[CodexRegistry] %s has no chatgpt_account_id, using static model list", account.Email)
			return codexSubscriptionModels()
		}
	}

	endpoint := codexBaseURL(account) + "/backend-api/codex/models"
	u, err := url.Parse(endpoint)
	if err != nil {
		logger.Warnf("[CodexRegistry] Failed to parse endpoint URL: %v, using static model list", err)
		return codexSubscriptionModels()
	}
	q := u.Query()
	q.Set("client_version", codexRegistryClientVersion)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		logger.Warnf("[CodexRegistry] Failed to create request: %v, using static model list", err)
		return codexSubscriptionModels()
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		logger.Warnf("[CodexRegistry] Request failed: %v, using static model list", err)
		return codexSubscriptionModels()
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warnf("[CodexRegistry] Failed to read response body: %v, using static model list", err)
		return codexSubscriptionModels()
	}

	if resp.StatusCode != 200 {
		logger.Warnf("[CodexRegistry] HTTP %d: %s, using static model list", resp.StatusCode, truncateErrBody(body))
		return codexSubscriptionModels()
	}

	var registry codexRegistryResponse
	if err := json.Unmarshal(body, &registry); err != nil {
		logger.Warnf("[CodexRegistry] Failed to parse JSON: %v, using static model list", err)
		return codexSubscriptionModels()
	}

	if len(registry.Models) == 0 {
		logger.Warnf("[CodexRegistry] Registry returned empty model list, using static model list")
		return codexSubscriptionModels()
	}

	// Convert upstream entries to ModelInfo, keeping only visible models
	// (visibility == "list"). Hidden models (gpt-reserve, codex-auto-review)
	// are internal/test models the user should not select.
	models := make([]ModelInfo, 0, len(registry.Models))
	for _, entry := range registry.Models {
		if entry.Visibility != "list" {
			continue
		}
		if entry.Slug == "" {
			continue
		}
		m := ModelInfo{
			ModelId:     entry.Slug,
			ModelName:   entry.DisplayName,
			Description: entry.Description,
			InputTypes:  []string{"text", "image"},
			Provider:    "openai-codex",
		}
		if m.ModelName == "" {
			m.ModelName = entry.Slug
		}
		if entry.ContextWindow > 0 {
			m.TokenLimits = &ModelTokenLimits{MaxInputTokens: entry.ContextWindow, MaxOutputTokens: 128000}
		}
		models = append(models, m)
	}

	if len(models) == 0 {
		logger.Warnf("[CodexRegistry] No visible models in registry, using static model list")
		return codexSubscriptionModels()
	}

	// Merge with static list: registry entries take precedence (newer metadata),
	// but static entries not in registry are preserved (e.g. o3, o4 which may
	// not be in the registry for this client_version).
	merged := mergeUniqueModels(models, codexSubscriptionModels())
	logger.Infof("[CodexRegistry] Discovered %d models (merged to %d with static fallback)", len(models), len(merged))
	return merged
}

// codexRegistryRefreshInterval is the minimum time between registry queries
// during refreshModelsCache. The upstream endpoint is not rate-limited as of
// 2026-09-07, but we cap at once per 10-minute refresh cycle to avoid
// unnecessary load and to batch discovery with other account refreshes.
const codexRegistryRefreshInterval = 10 * time.Minute
