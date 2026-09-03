package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newIamSsoServer serves the OIDC register / token / ListAvailableProfiles
// endpoints from one host. The production code builds
// https://oidc.<region>.amazonaws.com paths, so every request is rewritten onto
// this server and told apart by path.
func newIamSsoServer(t *testing.T, register, token, profiles authResponse) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		register.write(w)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		token.write(w)
	})
	// DiscoverProfileArn posts to the service root.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		profiles.write(w)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	installTestAuthClient(t, srv)
	return srv
}

func TestStartIamSsoLoginBuildsPKCEAuthorizeURL(t *testing.T) {
	newIamSsoServer(t,
		authResponse{body: `{"clientId":"sso-client","clientSecret":"sso-secret"}`},
		authResponse{}, authResponse{})

	sessionID, authorizeURL, expiresIn, err := StartIamSsoLogin("https://example.awsapps.com/start", "")
	if err != nil {
		t.Fatalf("StartIamSsoLogin: %v", err)
	}
	if sessionID == "" || expiresIn != 600 {
		t.Fatalf("session = (%q, %d), want an id and 600s", sessionID, expiresIn)
	}
	// An empty region must default to us-east-1 rather than producing
	// https://oidc..amazonaws.com.
	if !strings.HasPrefix(authorizeURL, "https://oidc.us-east-1.amazonaws.com/authorize?") {
		t.Fatalf("authorize URL = %q, want the us-east-1 OIDC host", authorizeURL)
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("client_id") != "sso-client" {
		t.Errorf("client_id = %q, want the registered client", q.Get("client_id"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Errorf("authorize URL missing PKCE challenge or state: %v", q)
	}
	if got := q.Get("scopes"); got != joinScopes() {
		t.Errorf("scopes = %q, want %q", got, joinScopes())
	}

	// The challenge must be the S256 hash of the stored verifier, otherwise the
	// later exchange is rejected by AWS.
	sessionsMu.RLock()
	session := sessions[sessionID]
	sessionsMu.RUnlock()
	if session == nil {
		t.Fatal("session was not stored")
	}
	if got := generateCodeChallenge(session.CodeVerifier); got != q.Get("code_challenge") {
		t.Fatalf("code_challenge %q does not match S256(verifier) %q", q.Get("code_challenge"), got)
	}
	if session.Region != "us-east-1" {
		t.Fatalf("session region = %q, want us-east-1", session.Region)
	}
}

func TestStartIamSsoLoginPropagatesRegistrationFailure(t *testing.T) {
	newIamSsoServer(t,
		authResponse{status: http.StatusForbidden, body: "not allowed"},
		authResponse{}, authResponse{})

	if _, _, _, err := StartIamSsoLogin("https://example.awsapps.com/start", "eu-central-1"); err == nil ||
		!strings.Contains(err.Error(), "register client failed") {
		t.Fatalf("error = %v, want register client failed", err)
	}
}

func TestCompleteIamSsoLoginExchangesCodeAndDiscoversProfile(t *testing.T) {
	const wantArn = "arn:aws:codewhisperer:us-east-1:111122223333:profile/ABCDEFGH"
	var exchanged map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"clientId":"sso-client","clientSecret":"sso-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&exchanged); err != nil {
			t.Errorf("decode token payload: %v", err)
		}
		_, _ = w.Write([]byte(`{"accessToken":"sso-at","refreshToken":"sso-rt","expiresIn":1800}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-amz-target"); got != "AmazonCodeWhispererService.ListAvailableProfiles" {
			t.Errorf("x-amz-target = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sso-at" {
			t.Errorf("Authorization = %q, want the freshly exchanged token", got)
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"profiles":[{"arn":%q}]}`, wantArn)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installTestAuthClient(t, srv)

	sessionID, authorizeURL, _, err := StartIamSsoLogin("https://example.awsapps.com/start", "us-east-1")
	if err != nil {
		t.Fatalf("StartIamSsoLogin: %v", err)
	}
	state := mustQueryParam(t, authorizeURL, "state")

	at, rt, clientID, clientSecret, region, expiresIn, profileArn, err := CompleteIamSsoLogin(
		sessionID, "http://127.0.0.1/oauth/callback?code=auth-code&state="+url.QueryEscape(state))
	if err != nil {
		t.Fatalf("CompleteIamSsoLogin: %v", err)
	}
	if at != "sso-at" || rt != "sso-rt" || expiresIn != 1800 {
		t.Fatalf("tokens = (%q, %q, %d)", at, rt, expiresIn)
	}
	if clientID != "sso-client" || clientSecret != "sso-secret" || region != "us-east-1" {
		t.Fatalf("client info = (%q, %q, %q)", clientID, clientSecret, region)
	}
	if profileArn != wantArn {
		t.Fatalf("profileArn = %q, want %q", profileArn, wantArn)
	}
	if exchanged["code"] != "auth-code" || exchanged["grantType"] != "authorization_code" {
		t.Fatalf("exchange payload = %v", exchanged)
	}
	if exchanged["codeVerifier"] == "" {
		t.Fatal("exchange payload omitted the PKCE verifier")
	}

	// The session is single-use: replaying the same callback must not mint a
	// second token pair.
	if _, _, _, _, _, _, _, err := CompleteIamSsoLogin(sessionID, "http://127.0.0.1/oauth/callback?code=auth-code&state="+url.QueryEscape(state)); err == nil {
		t.Fatal("a consumed session was accepted a second time")
	}
}

// A mismatched state is a CSRF signal and must abort before the code is
// exchanged.
func TestCompleteIamSsoLoginRejectsStateMismatchWithoutExchange(t *testing.T) {
	exchanges := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"clientId":"sso-client","clientSecret":"sso-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		exchanges++
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installTestAuthClient(t, srv)

	sessionID, _, _, err := StartIamSsoLogin("https://example.awsapps.com/start", "us-east-1")
	if err != nil {
		t.Fatalf("StartIamSsoLogin: %v", err)
	}

	_, _, _, _, _, _, _, err = CompleteIamSsoLogin(sessionID, "http://127.0.0.1/oauth/callback?code=c&state=attacker-state")
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("error = %v, want state mismatch", err)
	}
	if exchanges != 0 {
		t.Fatalf("token endpoint was called %d times despite a state mismatch", exchanges)
	}
}

func TestCompleteIamSsoLoginCallbackErrorCases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"clientId":"sso-client","clientSecret":"sso-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		t.Error("token endpoint must not be reached for an invalid callback")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installTestAuthClient(t, srv)

	newSession := func() (string, string) {
		t.Helper()
		id, authorizeURL, _, err := StartIamSsoLogin("https://example.awsapps.com/start", "us-east-1")
		if err != nil {
			t.Fatalf("StartIamSsoLogin: %v", err)
		}
		return id, mustQueryParam(t, authorizeURL, "state")
	}

	t.Run("provider error", func(t *testing.T) {
		id, state := newSession()
		_, _, _, _, _, _, _, err := CompleteIamSsoLogin(id,
			"http://127.0.0.1/oauth/callback?error=access_denied&state="+url.QueryEscape(state))
		if err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Fatalf("error = %v, want access_denied", err)
		}
	})

	t.Run("missing code", func(t *testing.T) {
		id, state := newSession()
		_, _, _, _, _, _, _, err := CompleteIamSsoLogin(id,
			"http://127.0.0.1/oauth/callback?state="+url.QueryEscape(state))
		if err == nil || !strings.Contains(err.Error(), "no authorization code") {
			t.Fatalf("error = %v, want no authorization code", err)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		_, _, _, _, _, _, _, err := CompleteIamSsoLogin("no-such-session",
			"http://127.0.0.1/oauth/callback?code=c&state=s")
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("error = %v, want a missing-session error", err)
		}
	})
}

// An expired session must be rejected and dropped, not exchanged.
func TestCompleteIamSsoLoginRejectsAndDropsExpiredSession(t *testing.T) {
	const sessionID = "expired-session"
	sessionsMu.Lock()
	sessions[sessionID] = &IamSsoSession{
		State:     "state-value",
		Region:    "us-east-1",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	sessionsMu.Unlock()

	_, _, _, _, _, _, _, err := CompleteIamSsoLogin(sessionID, "http://127.0.0.1/oauth/callback?code=c&state=state-value")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v, want an expiry error", err)
	}
	sessionsMu.RLock()
	_, still := sessions[sessionID]
	sessionsMu.RUnlock()
	if still {
		t.Fatal("expired session was left in the map")
	}
}

func TestCleanupExpiredSessionsKeepsLiveSessions(t *testing.T) {
	sessionsMu.Lock()
	sessions["cleanup-live"] = &IamSsoSession{ExpiresAt: time.Now().Add(10 * time.Minute)}
	sessions["cleanup-stale"] = &IamSsoSession{ExpiresAt: time.Now().Add(-10 * time.Minute)}
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, "cleanup-live")
		sessionsMu.Unlock()
	})

	cleanupExpiredSessions()

	sessionsMu.RLock()
	_, live := sessions["cleanup-live"]
	_, stale := sessions["cleanup-stale"]
	sessionsMu.RUnlock()
	if !live {
		t.Error("a live session was cleaned up")
	}
	if stale {
		t.Error("an expired session survived cleanup")
	}
}

func TestJoinScopesMatchesRequestedScopeSet(t *testing.T) {
	got := joinScopes()
	if got != strings.Join(scopes, ",") {
		t.Fatalf("joinScopes() = %q, want the comma-joined scope list", got)
	}
	for _, scope := range scopes {
		if !strings.Contains(got, scope) {
			t.Errorf("joinScopes() omitted %q", scope)
		}
	}
}

func TestGenerateCodeVerifierIsRandomURLSafeEntropy(t *testing.T) {
	first, second := generateCodeVerifier(), generateCodeVerifier()
	if first == second {
		t.Fatal("two consecutive verifiers were identical")
	}
	// 32 random bytes base64url-encoded without padding.
	if len(first) != 43 {
		t.Fatalf("verifier length = %d, want 43", len(first))
	}
	if strings.ContainsAny(first, "+/=") {
		t.Fatalf("verifier %q is not URL-safe", first)
	}
	if a, b := generateCodeChallenge(first), generateCodeChallenge(first); a != b {
		t.Fatal("code challenge is not deterministic for one verifier")
	}
	if generateCodeChallenge(first) == generateCodeChallenge(second) {
		t.Fatal("distinct verifiers produced the same challenge")
	}
}

func mustQueryParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	value := parsed.Query().Get(key)
	if value == "" {
		t.Fatalf("URL %q has no %q parameter", rawURL, key)
	}
	return value
}
