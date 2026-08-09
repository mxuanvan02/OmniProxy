package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// makeCodexJWT builds a minimal JWT with the chatgpt_account_id claim for
// testing extractCodexAccountID. The signature is fake (we only decode
// the payload).
func makeCodexJWT(accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{
		"sub": "user-123",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": accountID,
			"user_id":            "user-123",
		},
	})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadB64 + ".fakesig"
}

func makeCodexJWTWithExpiry(accountID string, expiresAt int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{
		"exp": expiresAt,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": accountID,
		},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

// TestExtractCodexAccountID verifies the JWT claim extraction.
func TestExtractCodexAccountID(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"valid", makeCodexJWT("acct_abc123"), "acct_abc123"},
		{"missing claim", makeJWTWithoutClaim(), ""},
		{"malformed", "not.a.jwt.at.all", ""},
		{"empty", "", ""},
		{"two parts", "abc.def", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCodexAccountID(c.token)
			if got != c.want {
				t.Errorf("extractCodexAccountID(%q) = %q, want %q", c.token, got, c.want)
			}
		})
	}
}

func makeJWTWithoutClaim() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{"sub": "user-123"})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadB64 + ".fakesig"
}

// TestExtractCodexAccountIDPublic verifies the exported wrapper.
func TestExtractCodexAccountIDPublic(t *testing.T) {
	if got := ExtractCodexAccountIDPublic(makeCodexJWT("acct_xyz")); got != "acct_xyz" {
		t.Errorf("ExtractCodexAccountIDPublic = %q, want acct_xyz", got)
	}
}

func TestExtractCodexJWTExpiry(t *testing.T) {
	want := int64(1_700_000_123)
	info := ExtractCodexJWTInfoPublic(makeCodexJWTWithExpiry("acct_exp", want))
	if info.AccountID != "acct_exp" || info.ExpiresAt != want {
		t.Fatalf("JWT info = %+v, want account acct_exp and expiry %d", info, want)
	}
	if got := ExtractCodexJWTInfoPublic(makeCodexJWT("acct_no_exp")).ExpiresAt; got != 0 {
		t.Fatalf("missing exp = %d, want 0", got)
	}
}

func TestRefreshCodexTokenAllowsMissingRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + makeCodexJWT("acct_refresh") + `","expires_in":3600}`))
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := &http.Client{Transport: rewriteAuthRequestTransport{target: target}}
	previous := SetGlobalAuthClientForTest(client)
	defer SetGlobalAuthClientForTest(previous)

	tokens, err := RefreshCodexToken("refresh-placeholder")
	if err != nil {
		t.Fatalf("refresh with omitted refresh_token failed: %v", err)
	}
	if tokens.AccessToken == "" || tokens.AccountID != "acct_refresh" {
		t.Fatalf("unexpected refresh result: %+v", tokens)
	}
	if tokens.RefreshToken != "" {
		t.Fatalf("expected omitted refresh token to remain empty for caller fallback, got %q", tokens.RefreshToken)
	}
}

func TestExchangeCodexCodeRequiresRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse authorization-code form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "authorization_code" {
			t.Fatalf("grant_type = %q, want authorization_code", got)
		}
		if got := r.Form.Get("code"); got != "authorization-code" {
			t.Fatalf("code = %q", got)
		}
		if got := r.Form.Get("code_verifier"); got != "pkce-verifier" {
			t.Fatalf("code_verifier = %q", got)
		}
		if got := r.Form.Get("redirect_uri"); got != codexRedirectURI {
			t.Fatalf("redirect_uri = %q, want %q", got, codexRedirectURI)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + makeCodexJWT("acct_no_refresh") + `","expires_in":3600}`))
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	previous := SetGlobalAuthClientForTest(&http.Client{Transport: rewriteAuthRequestTransport{target: target}})
	defer SetGlobalAuthClientForTest(previous)

	if _, err := exchangeCodexCode("authorization-code", "pkce-verifier"); err == nil || !strings.Contains(err.Error(), "authorization response missing refresh_token") {
		t.Fatalf("exchange error = %v, want missing refresh_token", err)
	}
}

func TestExchangeCodexCodeReturnsRefreshToken(t *testing.T) {
	const refreshToken = "refresh-from-login"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse authorization-code form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "authorization_code" {
			t.Fatalf("grant_type = %q, want authorization_code", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + makeCodexJWT("acct_login") + `","refresh_token":"` + refreshToken + `","expires_in":3600}`))
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	previous := SetGlobalAuthClientForTest(&http.Client{Transport: rewriteAuthRequestTransport{target: target}})
	defer SetGlobalAuthClientForTest(previous)

	tokens, err := exchangeCodexCode("authorization-code", "pkce-verifier")
	if err != nil {
		t.Fatalf("exchange authorization code: %v", err)
	}
	if tokens.RefreshToken != refreshToken || tokens.AccountID != "acct_login" {
		t.Fatalf("login tokens = %+v, want refresh token and account ID", tokens)
	}
}

type rewriteAuthRequestTransport struct {
	target *url.URL
}

func (t rewriteAuthRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// TestGenerateCodexState verifies state is 32 hex chars and unique.
func TestGenerateCodexState(t *testing.T) {
	s1 := generateCodexState()
	s2 := generateCodexState()
	if len(s1) != 32 {
		t.Errorf("state length = %d, want 32", len(s1))
	}
	if !isHex(s1) {
		t.Errorf("state %q is not hex", s1)
	}
	if s1 == s2 {
		t.Error("two consecutive states were identical (not random)")
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// TestStartCodexLoginURL verifies the authorize URL contains the required
// PKCE + Codex-specific params.
func TestStartCodexLoginURL(t *testing.T) {
	session, err := StartCodexLogin()
	if err != nil {
		// Port 1455 might be in use during testing — skip rather than fail.
		if strings.Contains(err.Error(), "callback server") {
			t.Skipf("port 1455 unavailable: %v", err)
		}
		t.Fatalf("StartCodexLogin: %v", err)
	}
	defer CancelCodexLogin()

	if session.AuthURL == "" {
		t.Fatal("AuthURL is empty")
	}
	if !strings.HasPrefix(session.AuthURL, "https://auth.openai.com/oauth/authorize?") {
		t.Errorf("AuthURL does not start with expected endpoint: %s", session.AuthURL)
	}
	mustContain := []string{
		"client_id=app_EMoamEEZ73f0CkXaXp7hrann",
		"response_type=code",
		"code_challenge_method=S256",
		"code_challenge=",
		"state=",
		"scope=openid+profile+email+offline_access",
		"redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback",
		"codex_cli_simplified_flow=true",
		"id_token_add_organizations=true",
	}
	for _, s := range mustContain {
		if !strings.Contains(session.AuthURL, s) {
			t.Errorf("AuthURL missing %q\nURL: %s", s, session.AuthURL)
		}
	}
	if session.Verifier == "" {
		t.Error("Verifier is empty")
	}
	if session.State == "" {
		t.Error("State is empty")
	}
}

func TestPersistentCodexCallbackWaitsForApprovedAccount(t *testing.T) {
	session, err := StartCodexLoginForAccount(t.TempDir(), "local-codex-account")
	if err != nil {
		if strings.Contains(err.Error(), "callback server") {
			t.Skipf("port 1455 unavailable: %v", err)
		}
		t.Fatalf("StartCodexLoginForAccount: %v", err)
	}
	defer CancelCodexLogin()

	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	statusURL := "http://127.0.0.1:1455/auth/callback/status?state=" + url.QueryEscape(session.State)
	readStatus := func() string {
		resp, err := client.Get(statusURL)
		if err != nil {
			t.Fatalf("read callback status: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("callback status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode callback status: %v", err)
		}
		return body.Status
	}

	if got := readStatus(); got != "pending" {
		t.Fatalf("callback status before account verification = %q, want pending", got)
	}
	CompleteCodexLogin()
	if got := readStatus(); got != "approved" {
		t.Fatalf("callback status after account verification = %q, want approved", got)
	}
}
