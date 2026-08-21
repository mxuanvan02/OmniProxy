package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"omniproxy/config"
)

// modelsServer spins up a fake OpenAI-compatible provider that only implements
// /v1/models, returning the supplied IDs.
func modelsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	// fetchExternalProviderModels resolves the outbound proxy from global config,
	// which panics when the config store was never initialised.
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A provider whose catalog has no "auto" alias must be pinged with a real model
// ID. Hard-coding "auto" made such providers answer HTTP 503 model_not_found.
func TestResolveExternalTestModelUsesFirstCatalogModel(t *testing.T) {
	srv := modelsServer(t, 200, `{"data":[{"id":"claude-opus-5"},{"id":"model-S"}]}`)
	account := &config.Account{
		ID: "ext-test-1", Email: "no-auto-provider", AuthMethod: externalAuthMethod,
		AccessToken: "sk-test", BaseURL: srv.URL,
	}
	if got := resolveExternalTestModel(account); got != "claude-opus-5" {
		t.Fatalf("expected first catalog model, got %q", got)
	}
}

// Providers that genuinely advertise "auto" keep using it, even when it is not
// the first entry, so aggregator routing is preserved.
func TestResolveExternalTestModelPrefersAdvertisedAuto(t *testing.T) {
	srv := modelsServer(t, 200, `{"data":[{"id":"model-S"},{"id":"auto"}]}`)
	account := &config.Account{
		ID: "ext-test-2", Email: "auto-provider", AuthMethod: externalAuthMethod,
		AccessToken: "sk-test", BaseURL: srv.URL,
	}
	if got := resolveExternalTestModel(account); got != "auto" {
		t.Fatalf("expected auto, got %q", got)
	}
}

// A provider with no usable catalog (error or empty list) falls back to "auto"
// rather than sending an empty model ID.
func TestResolveExternalTestModelFallsBackToAuto(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"catalog unavailable", 500, `{"error":"boom"}`},
		{"catalog empty", 200, `{"data":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, tc.status, tc.body)
			account := &config.Account{
				ID: "ext-test-3", Email: "no-catalog", AuthMethod: externalAuthMethod,
				AccessToken: "sk-test", BaseURL: srv.URL,
			}
			if got := resolveExternalTestModel(account); got != "auto" {
				t.Fatalf("expected auto fallback, got %q", got)
			}
		})
	}
}

func TestResolveExternalTestModelNilAccount(t *testing.T) {
	if got := resolveExternalTestModel(nil); got != "auto" {
		t.Fatalf("expected auto for nil account, got %q", got)
	}
}
