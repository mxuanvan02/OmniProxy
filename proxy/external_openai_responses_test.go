package proxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"omniproxy/config"
)

// initDialectTestConfig installs a minimal global config for the dispatch tests.
// dispatchChat reaches ResolveAccountProxyURL on every real adapter, and that
// dereferences the package-level cfg — nil until Init has run, which panics
// rather than returning an error. Other tests in this package follow the same
// idiom (see external_codex_test.go), so this is the established setup rather
// than a shortcut around the code under test.
func initDialectTestConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

func TestExternalAPIDialect(t *testing.T) {
	cases := []struct {
		name    string
		account *config.Account
		want    string
	}{
		{"nil account", nil, "chat"},
		{"empty", &config.Account{}, "chat"},
		{"chat", &config.Account{ExternalAPIDialect: "chat"}, "chat"},
		{"responses", &config.Account{ExternalAPIDialect: "responses"}, "responses"},
		{"responses uppercase", &config.Account{ExternalAPIDialect: "Responses"}, "responses"},
		{"responses padded", &config.Account{ExternalAPIDialect: "  responses  "}, "responses"},
		{"unknown value falls back to chat", &config.Account{ExternalAPIDialect: "grpc"}, "chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalAPIDialect(tc.account); got != tc.want {
				t.Fatalf("externalAPIDialect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExternalResponsesPath(t *testing.T) {
	cases := []struct {
		name    string
		account *config.Account
		want    string
	}{
		{"nil account", nil, "/v1/responses"},
		{"empty override", &config.Account{}, "/v1/responses"},
		{"override without slash", &config.Account{ResponsesPath: "v1/responses"}, "/v1/responses"},
		{"codex-style prefix", &config.Account{ResponsesPath: "/codex/responses"}, "/codex/responses"},
		{"whitespace only", &config.Account{ResponsesPath: "   "}, "/v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalResponsesPath(tc.account); got != tc.want {
				t.Fatalf("externalResponsesPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// The stub returns a distinguishable error, so this test proves the branch routes
// to the Responses adapter rather than the chat one without needing a live server.
func TestDispatchChatRoutesResponsesDialect(t *testing.T) {
	initDialectTestConfig(t)
	account := &config.Account{
		ID:                 "dialect-route",
		AuthMethod:         "external_openai",
		BaseURL:            "https://example.invalid",
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}
	err := dispatchChat(context.Background(), account, &KiroPayload{}, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected the responses stub to return an error")
	}
	if !strings.Contains(err.Error(), "responses dialect") {
		t.Fatalf("dispatchChat routed to the wrong adapter: %v", err)
	}
}

// dispatchChat picks an upstream through ordered predicates, and AgentRouter
// accounts satisfy isExternalAccount too. The Responses branch therefore has to
// sit after the AgentRouter arm; if it is moved earlier, every AgentRouter
// account that happens to carry the dialect field is silently rerouted and the
// positive routing test still passes. Pin the precedence by asserting the
// Responses stub is NOT what these accounts reach.
func TestDialectFieldDoesNotRerouteOtherAccountTypes(t *testing.T) {
	initDialectTestConfig(t)
	cases := []struct {
		name       string
		authMethod string
	}{
		{"agentrouter keeps its own adapter", "agentrouter"},
		{"codex keeps its own adapter", "codex"},
		{"antigravity keeps its own adapter", "antigravity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &config.Account{
				ID:                 "precedence-" + tc.authMethod,
				AuthMethod:         tc.authMethod,
				BaseURL:            "https://example.invalid",
				AccessToken:        "sk-test",
				ExternalAPIDialect: "responses",
			}
			err := dispatchChat(context.Background(), account, &KiroPayload{}, &KiroStreamCallback{})
			if err != nil && strings.Contains(err.Error(), "responses dialect") {
				t.Fatalf("%s account was rerouted into the Responses adapter: %v", tc.authMethod, err)
			}
		})
	}
}
