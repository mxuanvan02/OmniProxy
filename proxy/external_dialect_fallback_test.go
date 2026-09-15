package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
)

// dialectFallbackGateway answers the Responses path with a status the test
// chooses and the chat path with a complete stream, recording every path it was
// asked for so a test can assert which dialect the proxy actually used.
type dialectFallbackGateway struct {
	*httptest.Server
	responsesStatus int
	responsesBody   string
	paths           []string
}

func newDialectFallbackGateway(t *testing.T, responsesStatus int, responsesBody string) *dialectFallbackGateway {
	t.Helper()
	gw := &dialectFallbackGateway{responsesStatus: responsesStatus, responsesBody: responsesBody}
	gw.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw.paths = append(gw.paths, r.URL.Path)
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, chatFallbackStream())
			return
		}
		w.WriteHeader(gw.responsesStatus)
		fmt.Fprint(w, gw.responsesBody)
	}))
	t.Cleanup(gw.Close)
	return gw
}

func (g *dialectFallbackGateway) chatRequests() int {
	count := 0
	for _, p := range g.paths {
		if p == "/v1/chat/completions" {
			count++
		}
	}
	return count
}

// chatFallbackStream is the smallest chat-completions stream that the chat
// parser accepts as a finished turn.
func chatFallbackStream() string {
	return strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
}

// fallbackAccount registers a responses-dialect external account in the test
// config and returns it, because the fallback persists its correction through
// config and needs the account to exist there.
func fallbackAccount(t *testing.T, baseURL, id string) *config.Account {
	t.Helper()
	initDialectTestConfig(t)
	account := config.Account{
		ID:                 id,
		Email:              id + "@example.com",
		AuthMethod:         externalAuthMethod,
		BaseURL:            baseURL,
		AccessToken:        "sk-test",
		ExternalAPIDialect: "responses",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return &account
}

func storedDialect(t *testing.T, id string) string {
	t.Helper()
	for _, account := range config.GetAccounts() {
		if account.ID == id {
			return account.ExternalAPIDialect
		}
	}
	t.Fatalf("account %s not found in config", id)
	return ""
}

func fallbackCallback(text *strings.Builder) *KiroStreamCallback {
	produced := false
	return &KiroStreamCallback{
		OnText: func(s string, isThinking bool) {
			if !isThinking {
				text.WriteString(s)
			}
		},
		OnOutput:  func() { produced = true },
		HasOutput: func() bool { return produced },
	}
}

// TestExternalDialectFallsBackToChatOn404 covers the whole point of the phase:
// a gateway that has no Responses endpoint costs one failed request and then
// never costs another, because the account is corrected in config.
func TestExternalDialectFallsBackToChatOn404(t *testing.T) {
	gw := newDialectFallbackGateway(t, http.StatusNotFound, `{"error":{"message":"Not Found"}}`)
	account := fallbackAccount(t, gw.URL, "ext-fallback")
	payload := responsesDialectPayload()

	var text strings.Builder
	if err := dispatchChat(context.Background(), account, payload, fallbackCallback(&text)); err != nil {
		t.Fatalf("dispatchChat: %v", err)
	}
	if text.String() != "Hello" {
		t.Fatalf("text = %q, want Hello", text.String())
	}
	want := []string{"/v1/responses", "/v1/chat/completions"}
	if strings.Join(gw.paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", gw.paths, want)
	}
	if got := storedDialect(t, "ext-fallback"); got != "chat" {
		t.Fatalf("stored dialect = %q, want chat", got)
	}

	// The second request is the acceptance criterion: it must not touch
	// /v1/responses at all, which is what the reload is for.
	reloaded := &config.Account{}
	for _, candidate := range config.GetAccounts() {
		if candidate.ID == "ext-fallback" {
			*reloaded = candidate
		}
	}
	if err := dispatchChat(context.Background(), reloaded, responsesDialectPayload(), fallbackCallback(&strings.Builder{})); err != nil {
		t.Fatalf("second dispatchChat: %v", err)
	}
	if gw.chatRequests() != 2 {
		t.Fatalf("chat requests = %d, want 2", gw.chatRequests())
	}
	if len(gw.paths) != 3 {
		t.Fatalf("paths after second request = %v, want one more chat request only", gw.paths)
	}
}

// TestExternalDialectDoesNotFallBackOnAuthFailure pins the boundary: a 401 is
// the account's problem, not the dialect's, and the error has to reach the
// caller intact so the pool's auth handling still recognizes the status.
func TestExternalDialectDoesNotFallBackOnAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			gw := newDialectFallbackGateway(t, status, `{"error":{"message":"bad key"}}`)
			account := fallbackAccount(t, gw.URL, "ext-auth")

			var text strings.Builder
			err := dispatchChat(context.Background(), account, responsesDialectPayload(), fallbackCallback(&text))
			if err == nil {
				t.Fatal("expected the upstream error to surface")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
				t.Fatalf("error = %v, want the HTTP %d status preserved", err, status)
			}
			if strings.Contains(err.Error(), errExternalDialectUnsupported.Error()) {
				t.Fatalf("error = %v, must not be marked as an unsupported dialect", err)
			}
			if gw.chatRequests() != 0 {
				t.Fatalf("chat requests = %d, want 0", gw.chatRequests())
			}
			if got := storedDialect(t, "ext-auth"); got != "responses" {
				t.Fatalf("stored dialect = %q, want responses", got)
			}
		})
	}
}

// TestExternalDialectDoesNotFallBackAfterOutput checks the replay guard. The
// handler shares one callback across the attempts of a request, so HasOutput
// can already be true from an earlier attempt: replaying here would render the
// turn twice in front of the client.
func TestExternalDialectDoesNotFallBackAfterOutput(t *testing.T) {
	gw := newDialectFallbackGateway(t, http.StatusNotFound, `{"error":{"message":"Not Found"}}`)
	account := fallbackAccount(t, gw.URL, "ext-partial")

	callback := fallbackCallback(&strings.Builder{})
	callback.HasOutput = func() bool { return true }

	err := dispatchChat(context.Background(), account, responsesDialectPayload(), callback)
	if err == nil {
		t.Fatal("expected the upstream error to surface")
	}
	if gw.chatRequests() != 0 {
		t.Fatalf("chat requests = %d, want 0 after output was already delivered", gw.chatRequests())
	}
	if got := storedDialect(t, "ext-partial"); got != "responses" {
		t.Fatalf("stored dialect = %q, want responses", got)
	}
}

// TestExternalDialectDoesNotFallBackWithoutAProbe covers the missing-signal
// case: a callback that cannot report whether it produced output must not be
// treated as if it did not.
func TestExternalDialectDoesNotFallBackWithoutAProbe(t *testing.T) {
	gw := newDialectFallbackGateway(t, http.StatusNotFound, `{"error":{"message":"Not Found"}}`)
	account := fallbackAccount(t, gw.URL, "ext-noprobe")

	var text strings.Builder
	callback := &KiroStreamCallback{OnText: func(s string, isThinking bool) { text.WriteString(s) }}
	if err := dispatchChat(context.Background(), account, responsesDialectPayload(), callback); err == nil {
		t.Fatal("expected the upstream error to surface")
	}
	if gw.chatRequests() != 0 {
		t.Fatalf("chat requests = %d, want 0 when the callback cannot report output", gw.chatRequests())
	}
}

// TestExternalDialectUnsupportedNeedsTheRoute pinned by status and body. The
// model cases are the ones that matter: a gateway that answers "model not
// found" with a 404 is describing the request, and switching dialects over it
// would take a working account off the dialect its operator chose.
func TestExternalDialectUnsupportedNeedsTheRoute(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"404 bare", http.StatusNotFound, "", true},
		{"405 bare", http.StatusMethodNotAllowed, "", true},
		{"404 html page", http.StatusNotFound, "<html>Not Found</html>", true},
		{"404 model_not_found", http.StatusNotFound, `{"error":{"code":"model_not_found"}}`, false},
		{"404 unknown model in prose", http.StatusNotFound, `{"error":{"message":"The model gpt-9 does not exist"}}`, false},
		{"400 unknown url", http.StatusBadRequest, `{"error":{"message":"Unknown URL: /v1/responses"}}`, true},
		{"400 unsupported path", http.StatusBadRequest, `{"error":{"message":"unsupported path"}}`, true},
		{"400 malformed field", http.StatusBadRequest, `{"error":{"message":"input is required"}}`, false},
		{"400 model typo", http.StatusBadRequest, `{"error":{"message":"unknown model"}}`, false},
		{"500", http.StatusInternalServerError, "boom", false},
		{"429", http.StatusTooManyRequests, "slow down", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status}
			if got := externalResponsesDialectUnsupported(resp, []byte(tc.body)); got != tc.want {
				t.Fatalf("externalResponsesDialectUnsupported(%d, %q) = %v, want %v",
					tc.status, tc.body, got, tc.want)
			}
		})
	}
	if externalResponsesDialectUnsupported(nil, nil) {
		t.Fatal("a nil response must not read as an unsupported dialect")
	}
}
