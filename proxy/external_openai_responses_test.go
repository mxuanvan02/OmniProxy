package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// responsesDialectPayload is the Phase 01 golden payload with a model attached.
//
// The golden fixture deliberately leaves the model empty so it pins the Codex
// dialect's own default model. A generic gateway has no default to fall back on,
// so the builder rejects a model-less payload for this dialect by design — every
// test that wants to reach the network has to name a model.
func responsesDialectPayload() *KiroPayload {
	payload := goldenResponsesPayload()
	payload.OriginalModel = "gpt-5.6-terra"
	return payload
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

// dispatchChat must send a Responses-dialect account to the Responses path. The
// upstream records every path it is asked for, so this proves the routing rather
// than trusting an error string: a chat-dialect account reaching the same server
// would ask for /v1/chat/completions.
func TestDispatchChatRoutesResponsesDialect(t *testing.T) {
	initDialectTestConfig(t)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n")
	}))
	defer srv.Close()

	account := &config.Account{
		ID:                 "dialect-route",
		AuthMethod:         "external_openai",
		BaseURL:            srv.URL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}
	if err := dispatchChat(context.Background(), account, responsesDialectPayload(), &KiroStreamCallback{}); err != nil {
		t.Fatalf("dispatchChat: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/v1/responses" {
		t.Fatalf("upstream saw %v, want exactly [/v1/responses]", paths)
	}
}

// dispatchChat picks an upstream through ordered predicates, and AgentRouter
// accounts satisfy isExternalAccount too. The Responses branch therefore has to
// sit after the AgentRouter arm; if it is moved earlier, every AgentRouter
// account that happens to carry the dialect field is silently rerouted and the
// positive routing test still passes. Pin the precedence by asserting these
// accounts do NOT reach the Responses adapter.
//
// The discriminator is the Responses adapter's own error prefix
// ("external responses call"), which the chat path ("external call ") and the
// other adapters never emit. A path-recording server would not work here: the
// misrouted account fails model resolution before any request is sent, so no
// path would be recorded either way.
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
			if err != nil && strings.Contains(err.Error(), "external responses call") {
				t.Fatalf("%s account was rerouted into the Responses adapter: %v", tc.authMethod, err)
			}
		})
	}
}
