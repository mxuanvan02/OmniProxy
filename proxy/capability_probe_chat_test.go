package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"testing"
)

// The chat probe has to speak the account's own dialect: a Responses or
// Messages gateway answers a chat-completions POST with 404/400 even when its
// chat capability is healthy, and that failure reads on the account matrix as
// "no chat, no vision".

func TestProbeChatRequestBodyMatchesDialect(t *testing.T) {
	cases := []struct {
		name       string
		dialect    string
		wantFields []string
		wantAbsent []string
	}{
		{"responses uses input", "responses", []string{"input", "max_output_tokens"}, []string{"messages", "max_tokens"}},
		{"chat uses messages", "chat", []string{"messages", "max_tokens"}, []string{"input", "max_output_tokens"}},
		{"anthropic uses messages", "anthropic", []string{"messages", "max_tokens"}, []string{"input"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := probeChatRequestBody(tc.dialect, "gpt-5.6-sol")
			if body == nil {
				t.Fatal("probeChatRequestBody returned nil")
			}
			var decoded map[string]interface{}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if decoded["model"] != "gpt-5.6-sol" {
				t.Errorf("model = %v, want gpt-5.6-sol", decoded["model"])
			}
			for _, f := range tc.wantFields {
				if _, ok := decoded[f]; !ok {
					t.Errorf("%s body missing %q: %s", tc.dialect, f, body)
				}
			}
			for _, f := range tc.wantAbsent {
				if _, ok := decoded[f]; ok {
					t.Errorf("%s body should not carry %q: %s", tc.dialect, f, body)
				}
			}
		})
	}
}

// The Responses API rejects max_output_tokens below 16, so a "1 token" probe
// would 400 for a reason unrelated to reachability.
func TestProbeChatRequestBodyRespectsResponsesTokenFloor(t *testing.T) {
	body := probeChatRequestBody("responses", "gpt-5.6-sol")
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	tokens, ok := decoded["max_output_tokens"].(float64)
	if !ok || tokens < 16 {
		t.Fatalf("max_output_tokens = %v, want at least 16", decoded["max_output_tokens"])
	}
}

func TestExternalDialectProbePath(t *testing.T) {
	cases := []struct {
		name    string
		account *config.Account
		dialect string
		want    string
	}{
		{
			name:    "responses default",
			account: &config.Account{},
			dialect: "responses",
			want:    defaultExternalResponsesPath,
		},
		{
			name:    "responses override",
			account: &config.Account{ResponsesPath: "v1/resp"},
			dialect: "responses",
			want:    "/v1/resp",
		},
		{
			name:    "anthropic default",
			account: &config.Account{},
			dialect: "anthropic",
			want:    defaultExternalAnthropicPath,
		},
		{
			name:    "anthropic override",
			account: &config.Account{AnthropicPath: "/messages"},
			dialect: "anthropic",
			want:    "/messages",
		},
		{
			name:    "chat default",
			account: &config.Account{},
			dialect: "chat",
			want:    defaultExternalChatPath,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := externalDialectProbePath(tc.account, tc.dialect); got != tc.want {
				t.Fatalf("path = %q, want %q", got, tc.want)
			}
		})
	}
}

// The Messages API authenticates with x-api-key and rejects a request with no
// anthropic-version, so the probe must not send the Bearer-only shape.
func TestApplyChatProbeAuthAnthropicSetsMessagesHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/messages", nil)
	applyChatProbeAuth(req, &config.Account{}, "secret-token", "anthropic")

	if got := req.Header.Get("x-api-key"); got != "secret-token" {
		t.Errorf("x-api-key = %q, want the credential", got)
	}
	if got := req.Header.Get("anthropic-version"); got == "" {
		t.Error("anthropic-version header missing; the Messages API rejects the request without it")
	}
}

func TestApplyChatProbeAuthBearerForOpenAIDialects(t *testing.T) {
	for _, dialect := range []string{"chat", "responses"} {
		t.Run(dialect, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
			applyChatProbeAuth(req, &config.Account{}, "secret-token", dialect)
			if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Errorf("Authorization = %q, want Bearer credential", got)
			}
			if got := req.Header.Get("x-api-key"); got != "" {
				t.Errorf("x-api-key = %q, want it unset for the %s dialect", got, dialect)
			}
		})
	}
}

// End to end: a gateway that only answers the Responses dialect must be probed
// as reachable, which is what previously failed with the chat-shaped body.
func TestProbeAccountCapabilityChatUsesResponsesDialect(t *testing.T) {
	config.Init("")

	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer srv.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{
		ID:                     "acc-responses",
		AuthMethod:             "external_openai",
		BaseURL:                srv.URL,
		AccessToken:            "secret",
		Enabled:                true,
		ExternalAPIDialect:     "responses",
		DiscoveredCapabilities: []string{capabilityChat},
	}
	handler.pool.SetModelList(account.ID, []string{"gpt-5.6-sol"})

	result := handler.probeAccountCapability(account, capabilityChat)
	if !result.OK {
		t.Fatalf("probe not OK: status=%d detail=%q", result.Status, result.Detail)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("probed path = %q, want /v1/responses", gotPath)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &decoded); err != nil {
		t.Fatalf("probe body is not valid JSON: %v (%s)", err, gotBody)
	}
	if _, hasMessages := decoded["messages"]; hasMessages {
		t.Errorf("probe sent a chat-completions body to a Responses gateway: %s", gotBody)
	}
	if _, hasInput := decoded["input"]; !hasInput {
		t.Errorf("probe body missing Responses \"input\": %s", gotBody)
	}
}
