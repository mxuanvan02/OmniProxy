package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
)

// dialectTestServer stands in for a gateway. It answers the model-list fetch the
// handler spawns after saving the account, so the background goroutine stays on
// loopback instead of attempting a real lookup.
func dialectTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func importExternalProvider(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/external-provider", strings.NewReader(body))
	h.apiImportExternalProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

// storedAccount returns the account the handler persisted for baseURL. It comes
// back by value because GetAccounts already hands out copies.
func storedAccount(t *testing.T, baseURL string) config.Account {
	t.Helper()
	for _, a := range config.GetAccounts() {
		if a.BaseURL == baseURL {
			return a
		}
	}
	t.Fatalf("no account stored for baseUrl %q", baseURL)
	return config.Account{}
}

// The handler must normalise the dialect rather than trusting the client: the
// config file is hand-editable, so a value that reaches it by any path still has
// to mean something the adapter understands. An unrecognised value is dropped
// rather than stored, so the config never carries a field that reads as if it
// were in use.
func TestImportExternalProviderNormalizesDialect(t *testing.T) {
	cases := []struct {
		name string
		sent string
		want string
	}{
		{"responses", "responses", "responses"},
		{"responses uppercase", "Responses", "responses"},
		{"anthropic", "anthropic", "anthropic"},
		{"anthropic uppercase", "Anthropic", "anthropic"},
		{"chat explicit", "chat", ""},
		{"unknown value", "grpc", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := dialectTestServer(t)
			body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"d","externalApiDialect":"` + tc.sent + `"}`
			importExternalProvider(t, body)

			if got := storedAccount(t, srv.URL).ExternalAPIDialect; got != tc.want {
				t.Fatalf("ExternalAPIDialect = %q, want %q", got, tc.want)
			}
		})
	}
}

// AgentRouter shares this endpoint. The dialect field selects between OpenAI
// dialects, which AgentRouter does not speak, so it must not be persisted there.
func TestImportExternalProviderIgnoresDialectForAgentRouter(t *testing.T) {
	srv := dialectTestServer(t)
	body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"ar","authMethod":"agentrouter","externalApiDialect":"responses"}`
	importExternalProvider(t, body)

	if got := storedAccount(t, srv.URL).ExternalAPIDialect; got != "" {
		t.Fatalf("AgentRouter account kept dialect %q", got)
	}
}

// responsesPath only means something on the Responses dialect: storing it on a
// chat account would leave a field in the config that reads as if it were in use.
// The stored value is trimmed, because the adapter composes it into a URL.
func TestImportExternalProviderStoresResponsesPathOnlyForResponses(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		path    string
		want    string
	}{
		{"responses keeps a trimmed path", "responses", "  /codex/responses  ", "/codex/responses"},
		{"chat drops the path", "chat", "/codex/responses", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := dialectTestServer(t)
			body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"p","externalApiDialect":"` +
				tc.dialect + `","responsesPath":"` + tc.path + `"}`
			importExternalProvider(t, body)

			if got := storedAccount(t, srv.URL).ResponsesPath; got != tc.want {
				t.Fatalf("ResponsesPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// anthropicPath is the Messages counterpart of responsesPath, with the same rule:
// it only means something on its own dialect, and the stored value is trimmed
// because the adapter composes it into a URL.
func TestImportExternalProviderStoresAnthropicPathOnlyForAnthropic(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		path    string
		want    string
	}{
		{"anthropic keeps a trimmed path", "anthropic", "  /anthropic/v1/messages  ", "/anthropic/v1/messages"},
		{"responses drops the anthropic path", "responses", "/anthropic/v1/messages", ""},
		{"chat drops the anthropic path", "chat", "/anthropic/v1/messages", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := dialectTestServer(t)
			body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"ap","externalApiDialect":"` +
				tc.dialect + `","anthropicPath":"` + tc.path + `"}`
			importExternalProvider(t, body)

			if got := storedAccount(t, srv.URL).AnthropicPath; got != tc.want {
				t.Fatalf("AnthropicPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// The two path overrides are independent: a Responses account that arrives with
// an anthropic path must not have it stored, and vice versa. Getting this wrong
// leaves a field in the config that reads as if it were in use.
func TestImportExternalProviderKeepsTheTwoPathOverridesApart(t *testing.T) {
	srv := dialectTestServer(t)
	body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"both","externalApiDialect":"anthropic",` +
		`"anthropicPath":"/anthropic/v1/messages","responsesPath":"/codex/responses"}`
	importExternalProvider(t, body)

	account := storedAccount(t, srv.URL)
	if account.AnthropicPath != "/anthropic/v1/messages" {
		t.Fatalf("AnthropicPath = %q", account.AnthropicPath)
	}
	if account.ResponsesPath != "" {
		t.Fatalf("ResponsesPath = %q on an anthropic account", account.ResponsesPath)
	}
}

// The response body is what the UI reads back, so the saved dialect has to be
// observable there too — otherwise a working configuration looks like a silent
// no-op to whoever just filled in the form.
func TestImportExternalProviderReportsDialectToCaller(t *testing.T) {
	srv := dialectTestServer(t)
	body := `{"baseUrl":"` + srv.URL + `","apiKey":"sk-x","name":"r","externalApiDialect":"responses","responsesPath":"/codex/responses"}`
	rec := importExternalProvider(t, body)

	var resp struct {
		Account map[string]string `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Account["externalApiDialect"]; got != "responses" {
		t.Fatalf("response account.externalApiDialect = %q, want responses", got)
	}
}
