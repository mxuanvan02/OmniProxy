// Package auth — 9router db.json import.
//
// 9router stores all provider connections in ~/.9router/db.json under a
// top-level "providerConnections" array. Each entry has:
//   - id           (UUID)
//   - provider     ("codex" | "kiro" | "qwen" | "openrouter" | ...)
//   - authType     ("oauth" | "apikey")
//   - name         (display name)
//   - accessToken  / refreshToken / apiKey / expiresAt (ISO-8601)
//   - providerSpecificData (provider-specific blob)
//
// OmniProxy only imports the two provider types it can actually route:
//   - codex  → AuthMethod="codex",        ChatGPTAccountID from providerSpecificData.chatgptAccountId
//   - kiro   → AuthMethod="idc"/"social", ProfileArn     from providerSpecificData.profileArn
//
// Other providers (qwen, openrouter, ollama, vertex, ...) are skipped —
// they don't map to either the Kiro/AWS backend or the Codex /v1/responses
// backend, and importing them as external_openai would require per-provider
// base URL discovery which 9router doesn't expose cleanly.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NineRouterConnection is one entry in 9router's db.json providerConnections
// array. Only the fields OmniProxy needs are decoded; the rest are ignored.
type NineRouterConnection struct {
	ID                   string                 `json:"id"`
	Provider             string                 `json:"provider"`
	AuthType             string                 `json:"authType"`
	Name                 string                 `json:"name"`
	AccessToken          string                 `json:"accessToken"`
	RefreshToken         string                 `json:"refreshToken"`
	APIKey               string                 `json:"apiKey"`
	ExpiresAt            string                 `json:"expiresAt"` // ISO-8601
	ProviderSpecificData map[string]interface{} `json:"providerSpecificData"`
}

// NineRouterDB is the top-level shape of ~/.9router/db.json.
type NineRouterDB struct {
	ProviderConnections []NineRouterConnection `json:"providerConnections"`
}

// NineRouterImportedAccount is one account extracted from 9router, ready to
// be turned into a config.Account by the handler. The handler decides the
// final AuthMethod + dedup strategy based on Category.
type NineRouterImportedAccount struct {
	SourceID         string // 9router connection ID
	Provider         string // "codex" | "kiro"
	Name             string // display name from 9router
	AccessToken      string
	RefreshToken     string
	ExpiresAt        int64 // Unix seconds; 0 if unparseable
	ChatGPTAccountID string // codex only
	ProfileArn       string // kiro only
	PlanType         string // codex: "plus" | "free" | "pro"
}

// NineRouterImportResult is what ReadNineRouterDB returns to the handler.
type NineRouterImportResult struct {
	Codex []NineRouterImportedAccount
	Kiro  []NineRouterImportedAccount
	Skipped []string // provider names that were skipped
	Path   string    // db.json path that was read
}

// nineRouterDBPath returns the default ~/.9router/db.json path. Can be
// overridden by the 9ROUTER_DB env var for testing.
func nineRouterDBPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("NINEROUTER_DB")); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("9router import: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, ".9router", "db.json"), nil
}

// ReadNineRouterDB reads ~/.9router/db.json and extracts codex + kiro
// accounts. Other providers are listed in result.Skipped.
//
// The caller (handler) is responsible for dedup, token refresh, and
// config.AddAccount — this function only parses + normalizes.
func ReadNineRouterDB() (*NineRouterImportResult, error) {
	path, err := nineRouterDBPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("9router import: read %s: %w", path, err)
	}
	var db NineRouterDB
	if err := json.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("9router import: parse %s: %w", path, err)
	}

	result := &NineRouterImportResult{Path: path}
	skipped := map[string]bool{}

	for _, conn := range db.ProviderConnections {
		switch conn.Provider {
		case "codex":
			acc := parseNineRouterCodex(conn)
			if acc != nil {
				result.Codex = append(result.Codex, *acc)
			} else {
				skipped["codex (invalid)"] = true
			}
		case "kiro":
			acc := parseNineRouterKiro(conn)
			if acc != nil {
				result.Kiro = append(result.Kiro, *acc)
			} else {
				skipped["kiro (invalid)"] = true
			}
		default:
			skipped[conn.Provider] = true
		}
	}

	for name := range skipped {
		result.Skipped = append(result.Skipped, name)
	}
	return result, nil
}

// parseNineRouterCodex extracts a Codex account from a 9router connection.
// Returns nil if the connection is missing required fields (access token +
// chatgptAccountId).
func parseNineRouterCodex(conn NineRouterConnection) *NineRouterImportedAccount {
	if strings.TrimSpace(conn.AccessToken) == "" {
		return nil
	}
	accountID := ""
	planType := ""
	if conn.ProviderSpecificData != nil {
		if v, ok := conn.ProviderSpecificData["chatgptAccountId"].(string); ok {
			accountID = strings.TrimSpace(v)
		}
		if v, ok := conn.ProviderSpecificData["chatgptPlanType"].(string); ok {
			planType = strings.TrimSpace(v)
		}
	}
	// chatgptAccountId may also be embedded in the JWT; the handler will
	// re-extract it via ExtractCodexAccountIDPublic if this is empty.
	return &NineRouterImportedAccount{
		SourceID:         conn.ID,
		Provider:         "codex",
		Name:             conn.Name,
		AccessToken:      strings.TrimSpace(conn.AccessToken),
		RefreshToken:     strings.TrimSpace(conn.RefreshToken),
		ExpiresAt:        parseNineRouterExpiry(conn.ExpiresAt),
		ChatGPTAccountID: accountID,
		PlanType:         planType,
	}
}

// parseNineRouterKiro extracts a Kiro account from a 9router connection.
// Returns nil if the connection is missing a refresh token (without one,
// we can't refresh to get a valid access token + profileArn).
func parseNineRouterKiro(conn NineRouterConnection) *NineRouterImportedAccount {
	if strings.TrimSpace(conn.RefreshToken) == "" {
		return nil
	}
	profileArn := ""
	if conn.ProviderSpecificData != nil {
		if v, ok := conn.ProviderSpecificData["profileArn"].(string); ok {
			profileArn = strings.TrimSpace(v)
		}
	}
	return &NineRouterImportedAccount{
		SourceID:     conn.ID,
		Provider:     "kiro",
		Name:         conn.Name,
		AccessToken:  strings.TrimSpace(conn.AccessToken),
		RefreshToken: strings.TrimSpace(conn.RefreshToken),
		ExpiresAt:    parseNineRouterExpiry(conn.ExpiresAt),
		ProfileArn:   profileArn,
	}
}

// parseNineRouterExpiry parses 9router's ISO-8601 expiresAt string into
// Unix seconds. Returns 0 if the string is empty or unparseable (the
// handler will trigger a refresh in that case).
func parseNineRouterExpiry(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 9router uses RFC 3339 with nanosecond precision (e.g.
	// "2026-05-24T09:12:38.945Z"). time.Parse handles both.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Try without sub-seconds.
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
	}
	return t.Unix()
}
