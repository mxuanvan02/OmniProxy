package proxy

import (
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"testing"
)

// TestCodexRegistryDiscoveryReturnsGPT6Astra verifies the proxy fetches the
// live model registry from OpenAI and includes models not in the static list.
// gpt-6-astra is the flagship model as of 2026-09, so its presence proves the
// discovery path works end-to-end.
func TestCodexRegistryDiscoveryReturnsGPT6Astra(t *testing.T) {
	// Mock upstream registry that returns gpt-6-astra (not in the static list)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/backend-api/codex/models") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": [
				{
					"slug": "gpt-6-astra",
					"display_name": "GPT-6-Astra",
					"description": "Flagship model",
					"context_window": 272000,
					"visibility": "list",
					"supported_reasoning_levels": [
						{"effort": "low", "description": "Fast"},
						{"effort": "medium", "description": "Balanced"},
						{"effort": "high", "description": "Deep"},
						{"effort": "xhigh", "description": "Extra high"},
						{"effort": "max", "description": "Maximum"},
						{"effort": "ultra", "description": "Ultimate"}
					]
				},
				{
					"slug": "gpt-5.6-sol",
					"display_name": "GPT-5.6-Sol",
					"description": "Previous flagship",
					"context_window": 272000,
					"visibility": "list",
					"supported_reasoning_levels": [
						{"effort": "low", "description": "Fast"},
						{"effort": "medium", "description": "Balanced"},
						{"effort": "high", "description": "Deep"}
					]
				}
			]
		}`))
	}))
	defer upstream.Close()

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}

	account := config.Account{
		ID:               "codex-test",
		Email:            "test@example.test",
		AuthMethod:       codexAuthMethod,
		AccessToken:      "fake-token",
		ChatGPTAccountID: "fake-account-id",
		BaseURL:          upstream.URL,
		Enabled:          true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	discovered := fetchCodexRegistryModels(&account)

	found := false
	for _, m := range discovered {
		if m.ModelId == "gpt-6-astra" {
			found = true
			if m.TokenLimits == nil || m.TokenLimits.MaxInputTokens != 272000 {
				t.Errorf("gpt-6-astra TokenLimits = %+v, want MaxInputTokens=272000", m.TokenLimits)
			}
			if m.ModelName == "" {
				t.Error("gpt-6-astra ModelName should not be empty")
			}
			break
		}
	}
	if !found {
		t.Errorf("gpt-6-astra not found in discovered models: %v", discovered)
	}
}

// TestCodexRegistryDiscoveryFallsBackToStaticList verifies the proxy falls back
// to the hardcoded list when the upstream registry is unreachable.
func TestCodexRegistryDiscoveryFallsBackToStaticList(t *testing.T) {
	// Mock upstream that returns 500
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error": "internal error"}`))
	}))
	defer upstream.Close()

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}

	account := config.Account{
		ID:               "codex-test",
		Email:            "test@example.test",
		AuthMethod:       codexAuthMethod,
		AccessToken:      "fake-token",
		ChatGPTAccountID: "fake-account-id",
		BaseURL:          upstream.URL,
		Enabled:          true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	discovered := fetchCodexRegistryModels(&account)

	// Should return at least the static models (gpt-5.6-sol, gpt-5.5, etc.)
	if len(discovered) == 0 {
		t.Error("fallback should return static models, got empty list")
	}

	// Verify at least one known static model is present
	foundGPT56Sol := false
	for _, m := range discovered {
		if m.ModelId == "gpt-5.6-sol" {
			foundGPT56Sol = true
			break
		}
	}
	if !foundGPT56Sol {
		t.Errorf("fallback missing gpt-5.6-sol: %v", discovered)
	}
}
