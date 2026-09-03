package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setAntigravityCreds installs fake desktop-client credentials for the duration
// of the test. Real values belong to Google's IDE and are never used in tests.
func setAntigravityCreds(t *testing.T) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_CLIENT_ID", "test-client-id.apps.googleusercontent.com")
	t.Setenv("ANTIGRAVITY_CLIENT_SECRET", "test-client-secret-xxx")
}

func TestAntigravityClientCredsPrefersEnvironment(t *testing.T) {
	setAntigravityCreds(t)
	id, secret, err := antigravityClientCreds()
	if err != nil {
		t.Fatalf("antigravityClientCreds: %v", err)
	}
	if id != "test-client-id.apps.googleusercontent.com" || secret != "test-client-secret-xxx" {
		t.Fatalf("creds = (%q, %q)", id, secret)
	}
}

// A login with no configured client must fail locally with an actionable
// message. Sending an empty client_id to Google returns an opaque error the
// operator cannot act on.
func TestAntigravityClientCredsRequiresBothValues(t *testing.T) {
	initTempAuthConfig(t)
	cases := [][2]string{{"", ""}, {"id-only", ""}, {"", "secret-only"}}
	for _, c := range cases {
		t.Setenv("ANTIGRAVITY_CLIENT_ID", c[0])
		t.Setenv("ANTIGRAVITY_CLIENT_SECRET", c[1])
		_, _, err := antigravityClientCreds()
		if err == nil || !strings.Contains(err.Error(), "no OAuth client configured") {
			t.Fatalf("creds(%q, %q) error = %v, want a configuration error", c[0], c[1], err)
		}
	}
}

func TestRefreshAntigravityTokenRejectsEmptyRefreshTokenWithoutRequest(t *testing.T) {
	setAntigravityCreds(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	for _, token := range []string{"", "   "} {
		if _, err := RefreshAntigravityToken(token); err == nil ||
			!strings.Contains(err.Error(), "empty refresh token") {
			t.Fatalf("RefreshAntigravityToken(%q) error = %v, want empty refresh token", token, err)
		}
	}
	if calls != 0 {
		t.Fatalf("upstream was called %d times for an empty refresh token, want 0", calls)
	}
}

func TestRefreshAntigravityTokenParsesResponseAndIdentity(t *testing.T) {
	setAntigravityCreds(t)
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		form = r.Form
		body := map[string]any{
			"access_token": "ag-access",
			"expires_in":   1800,
			"id_token":     makeGoogleIDToken(t, "ops@example.com", "Ops Person", "sub-123"),
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	tokens, err := RefreshAntigravityToken("ag-refresh")
	if err != nil {
		t.Fatalf("RefreshAntigravityToken: %v", err)
	}
	if tokens.AccessToken != "ag-access" {
		t.Fatalf("access token = %q", tokens.AccessToken)
	}
	// Google does not rotate the refresh token on this grant; an empty value
	// tells the caller to keep the stored one.
	if tokens.RefreshToken != "" {
		t.Fatalf("refresh token = %q, want empty so the caller keeps the stored one", tokens.RefreshToken)
	}
	if tokens.Email != "ops@example.com" || tokens.Name != "Ops Person" || tokens.Subject != "sub-123" {
		t.Fatalf("identity = %+v, want the id_token claims", tokens)
	}
	if delta := tokens.ExpiresAt - time.Now().Unix(); delta < 1790 || delta > 1800 {
		t.Fatalf("expiresAt is %ds away, want about 1800s", delta)
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := form.Get("refresh_token"); got != "ag-refresh" {
		t.Fatalf("refresh_token = %q", got)
	}
	if form.Get("client_id") == "" || form.Get("client_secret") == "" {
		t.Fatalf("client credentials were not sent: %v", form)
	}
}

// makeGoogleIDToken builds an unsigned JWT carrying the display claims. The
// signature is never verified — the token arrives from Google over TLS and is
// used for display only.
func makeGoogleIDToken(t *testing.T, email, name, subject string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"email": email, "name": name, "sub": subject})
	if err != nil {
		t.Fatalf("marshal id_token claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".unsigned"
}

// The authorization_code grant is the only place a refresh token is ever
// issued. A response without one must fail loudly: persisting the account
// would leave a credential that cannot survive its first hour.
func TestExchangeAntigravityCodeRequiresRefreshToken(t *testing.T) {
	setAntigravityCreds(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse exchange form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "authorization_code" {
			t.Errorf("grant_type = %q, want authorization_code", got)
		}
		_, _ = w.Write([]byte(`{"access_token":"ag-access","expires_in":3600}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	_, err := exchangeAntigravityCode("auth-code", "verifier", "http://localhost:51121/oauth-callback")
	if err == nil || !strings.Contains(err.Error(), "missing refresh_token") {
		t.Fatalf("error = %v, want missing refresh_token", err)
	}
}

func TestExchangeAntigravityCodeSendsPKCEAndReturnsTokens(t *testing.T) {
	setAntigravityCreds(t)
	const redirect = "http://localhost:51121/oauth-callback"
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse exchange form: %v", err)
		}
		form = r.Form
		_, _ = w.Write([]byte(`{"access_token":"ag-access","refresh_token":"ag-refresh","expires_in":0}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	tokens, err := exchangeAntigravityCode("auth-code", "pkce-verifier", redirect)
	if err != nil {
		t.Fatalf("exchangeAntigravityCode: %v", err)
	}
	if tokens.AccessToken != "ag-access" || tokens.RefreshToken != "ag-refresh" {
		t.Fatalf("tokens = %+v", tokens)
	}
	// expires_in=0 must fall back to an hour rather than producing a token that
	// is already expired at the moment it is stored.
	if delta := tokens.ExpiresAt - time.Now().Unix(); delta < 3590 || delta > 3600 {
		t.Fatalf("expiresAt is %ds away, want the 3600s default", delta)
	}
	for key, want := range map[string]string{
		"code":          "auth-code",
		"code_verifier": "pkce-verifier",
		"redirect_uri":  redirect,
	} {
		if got := form.Get(key); got != want {
			t.Errorf("form[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestDecodeGoogleIDTokenReadsClaims(t *testing.T) {
	info := DecodeGoogleIDToken(makeGoogleIDToken(t, "user@example.com", "User", "sub-9"))
	if info.Email != "user@example.com" || info.Name != "User" || info.Subject != "sub-9" {
		t.Fatalf("info = %+v", info)
	}
}

// Malformed tokens must decode to an empty identity rather than panicking:
// imported credentials routinely carry no id_token at all.
func TestDecodeGoogleIDTokenToleratesMalformedInput(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":`))
	cases := map[string]string{
		"empty":          "",
		"not a jwt":      "just-a-string",
		"two segments":   "header.payload",
		"four segments":  "a.b.c.d",
		"bad base64":     "header.!!!not-base64!!!.sig",
		"bad json claim": "header." + payload + ".sig",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if info := DecodeGoogleIDToken(token); info != (GoogleIDTokenInfo{}) {
				t.Fatalf("info = %+v, want zero value", info)
			}
		})
	}
}

// google-auth-library writes standard base64 with padding in some versions;
// both encodings must decode or the account shows up with no email.
func TestDecodeGoogleIDTokenAcceptsPaddedBase64(t *testing.T) {
	payload, err := json.Marshal(map[string]string{"email": "padded@example.com"})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	token := "header." + base64.URLEncoding.EncodeToString(payload) + ".sig"
	if got := DecodeGoogleIDToken(token).Email; got != "padded@example.com" {
		t.Fatalf("email = %q, want padded@example.com", got)
	}
}

func TestReadAntigravityCredsFileParsesBothExpiryFormats(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)

	msPath := filepath.Join(dir, "ms.json")
	writeJSONFile(t, msPath, map[string]any{
		"access_token":  "at",
		"refresh_token": "rt",
		"expiry_date":   expiry.UnixMilli(),
		"id_token":      makeGoogleIDToken(t, "ms@example.com", "MS", "sub-ms"),
	})
	creds, err := readAntigravityCredsFile(msPath)
	if err != nil {
		t.Fatalf("readAntigravityCredsFile(expiry_date): %v", err)
	}
	if creds.ExpiresAt != expiry.Unix() {
		t.Fatalf("expiry_date → ExpiresAt = %d, want %d", creds.ExpiresAt, expiry.Unix())
	}
	if creds.Email != "ms@example.com" || creds.RefreshToken != "rt" || creds.Path != msPath {
		t.Fatalf("creds = %+v", creds)
	}

	rfcPath := filepath.Join(dir, "rfc.json")
	writeJSONFile(t, rfcPath, map[string]any{
		"refresh_token": "rt",
		"expiry":        expiry.Format(time.RFC3339),
	})
	creds, err = readAntigravityCredsFile(rfcPath)
	if err != nil {
		t.Fatalf("readAntigravityCredsFile(expiry): %v", err)
	}
	if creds.ExpiresAt != expiry.Unix() {
		t.Fatalf("expiry → ExpiresAt = %d, want %d", creds.ExpiresAt, expiry.Unix())
	}
}

// A credential file without a refresh token cannot be imported: it would
// produce an account that dies at the first refresh.
func TestReadAntigravityCredsFileRejectsFileWithoutRefreshToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	writeJSONFile(t, path, map[string]any{"access_token": "at", "refresh_token": "   "})
	if _, err := readAntigravityCredsFile(path); err == nil ||
		!strings.Contains(err.Error(), "no refresh_token") {
		t.Fatalf("error = %v, want no refresh_token", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write malformed creds: %v", err)
	}
	if _, err := readAntigravityCredsFile(bad); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error = %v, want a parse failure", err)
	}
}

// ReadLocalAntigravityCreds walks a fixed candidate list, most specific first.
func TestReadLocalAntigravityCredsPrefersMostSpecificPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeJSONFile(t, mkdirFor(t, filepath.Join(home, ".gemini", "oauth_creds.json")),
		map[string]any{"refresh_token": "generic-rt"})
	writeJSONFile(t, mkdirFor(t, filepath.Join(home, ".gemini", "antigravity", "oauth_creds.json")),
		map[string]any{"refresh_token": "antigravity-rt"})
	writeJSONFile(t, mkdirFor(t, filepath.Join(home, ".gemini", "projects.json")),
		map[string]any{"/work/repo": map[string]string{"projectId": "gcp-project-1"}})

	creds, err := ReadLocalAntigravityCreds()
	if err != nil {
		t.Fatalf("ReadLocalAntigravityCreds: %v", err)
	}
	if creds.RefreshToken != "antigravity-rt" {
		t.Fatalf("refresh token = %q, want the antigravity-specific file to win", creds.RefreshToken)
	}
	if creds.ProjectID != "gcp-project-1" {
		t.Fatalf("project ID = %q, want the value cached by the local client", creds.ProjectID)
	}
}

func TestReadLocalAntigravityCredsReportsMissingCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := ReadLocalAntigravityCreds(); err == nil ||
		!strings.Contains(err.Error(), "no Antigravity credentials found") {
		t.Fatalf("error = %v, want a not-found error", err)
	}
}

func mkdirFor(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	return path
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRefreshAntigravityTokenFailureModes(t *testing.T) {
	setAntigravityCreds(t)
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"revoked refresh token", http.StatusUnauthorized, `{"error":"invalid_grant"}`, "HTTP 401"},
		{"server error", http.StatusInternalServerError, "boom", "HTTP 500"},
		{"malformed json", http.StatusOK, `{"access_token":`, "parse"},
		{"missing access token", http.StatusOK, `{"expires_in":3600}`, "missing access_token"},
		{"empty body", http.StatusOK, "", "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			installTestAuthClient(t, srv)

			_, err := RefreshAntigravityToken("ag-refresh")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "antigravity refresh") {
				t.Fatalf("error = %v, want it tagged as an antigravity refresh failure", err)
			}
		})
	}
}
