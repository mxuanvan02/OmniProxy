package auth

import (
	"encoding/base64"
	"encoding/json"
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
