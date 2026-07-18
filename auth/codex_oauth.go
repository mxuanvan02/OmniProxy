// Package auth: codex_oauth.go
//
// OpenAI Codex (ChatGPT subscription) OAuth 2.0 + PKCE login flow.
//
// This is the same flow the official Codex CLI uses for interactive sign-in:
//   - Client ID:  app_EMoamEEZ73f0CkXaXp7hrann  (OpenAI's public Codex client)
//   - Authorize:  https://auth.openai.com/oauth/authorize
//   - Token:      https://auth.openai.com/oauth/token
//   - Redirect:   http://localhost:1455/auth/callback  (port fixed by OpenAI)
//   - Scopes:     openid profile email offline_access
//
// The proxy starts a local HTTP server on port 1455, opens the user's browser
// to the authorize URL, captures the authorization code on callback, and
// exchanges it for an access_token (JWT) + refresh_token. The ChatGPT
// account_id is extracted from the JWT's
//   payload["https://api.openai.com/auth"].chatgpt_account_id
// claim and sent as the `chatgpt-account-id` header on every /v1/responses
// call to the Codex backend.
//
// Refresh uses the standard refresh_token grant against the same token URL.
//
// Reference: openai/codex (codex-rs), paoloanzn/free-code, chinmaymk/ra.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Public Codex OAuth constants (extracted from the official Codex CLI binary
// and confirmed by multiple third-party reimplementations). These are public
// values shipped in every Codex CLI install; they are not secrets.
const (
	codexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexRedirectURI  = "http://localhost:1455/auth/callback"
	codexScopes       = "openid profile email offline_access"
	codexCallbackPort = 1455

	// JWT claim namespace where OpenAI places the chatgpt_account_id.
	codexJWTAuthClaim    = "https://api.openai.com/auth"
	codexJWTProfileClaim = "https://api.openai.com/profile"
)

// CodexTokens holds the result of a successful Codex OAuth exchange or refresh.
type CodexTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix seconds
	AccountID    string // chatgpt_account_id extracted from the JWT
}

// CodexLoginSession holds the in-flight state of a Codex PKCE login.
// Only one Codex login can be active at a time because OpenAI registers a
// fixed redirect port (1455); concurrent logins would collide on the port.
type CodexLoginSession struct {
	Verifier    string
	State       string
	AuthURL     string
	StartedAt   time.Time
	ExpiresAt   time.Time
	codeChan    chan string
	errChan     chan error
	server      *http.Server
}

var (
	codexLoginMu      sync.Mutex
	codexLoginCurrent *CodexLoginSession
)

// generateCodeVerifier / generateCodeChallenge / generateState are shared
// with iam_sso.go (which declares the first two). We only need a Codex-
// specific state generator here.
func generateCodexState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// StartCodexLogin begins a PKCE login flow.
//
// It starts a local HTTP server on port 1455 to receive the OAuth callback,
// builds the authorize URL, and returns a session the caller can poll with
// PollCodexLogin. The caller must open AuthURL in the user's browser.
//
// Only one Codex login may be active at a time. Calling StartCodexLogin while
// another session is in flight returns an error.
func StartCodexLogin() (*CodexLoginSession, error) {
	codexLoginMu.Lock()
	defer codexLoginMu.Unlock()

	if codexLoginCurrent != nil && time.Now().Before(codexLoginCurrent.ExpiresAt) {
		// Abort the previous session — the user clicked login again.
		codexLoginCurrent.abort()
	}

	verifier := generateCodeVerifier()
	state := generateCodexState()
	challenge := generateCodeChallenge(verifier)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", codexClientID)
	q.Set("redirect_uri", codexRedirectURI)
	q.Set("scope", codexScopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	// OpenAI-specific params matched from the official Codex CLI.
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "omniproxy")

	authURL := codexAuthorizeURL + "?" + q.Encode()

	session := &CodexLoginSession{
		Verifier:  verifier,
		State:     state,
		AuthURL:   authURL,
		StartedAt: time.Now(),
		ExpiresAt: time.Now().Add(10 * time.Minute),
		codeChan:  make(chan string, 1),
		errChan:   make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if errStr := q.Get("error"); errStr != "" {
			desc := q.Get("error_description")
			select {
			case session.errChan <- fmt.Errorf("oauth error: %s: %s", errStr, desc):
			default:
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "Login failed: %s", errStr)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		select {
		case session.codeChan <- code:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "<html><body><h2>✅ OpenAI authentication completed.</h2><p>You can close this window and return to OmniProxy.</p></body></html>")
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", codexCallbackPort),
		Handler: mux,
	}
	// Use a short idle timeout so the callback server doesn't leak sockets.
	srv.IdleTimeout = 30 * time.Second

	listenErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			listenErr <- err
		}
	}()

	// Wait briefly for either successful listen or a bind error.
	select {
	case err := <-listenErr:
		return nil, fmt.Errorf("codex login: callback server on port %d: %w (is another Codex login or Codex CLI already running?)", codexCallbackPort, err)
	case <-time.After(100 * time.Millisecond):
		// server is listening
	}

	session.server = srv
	codexLoginCurrent = session
	return session, nil
}

// abort closes the in-flight session's HTTP server and drains channels.
// Caller must hold codexLoginMu.
func (s *CodexLoginSession) abort() {
	if s == nil {
		return
	}
	if s.server != nil {
		_ = s.server.Close()
		s.server = nil
	}
	select {
	case <-s.codeChan:
	default:
	}
	select {
	case <-s.errChan:
	default:
	}
}

// PollCodexLogin waits for the OAuth callback (or timeout) and exchanges the
// authorization code for tokens. Returns CodexTokens on success.
//
// If the user hasn't completed browser auth yet, returns an "authorization_pending" error.
// If the login session has expired or been replaced, returns an error.
func PollCodexLogin() (*CodexTokens, error) {
	codexLoginMu.Lock()
	session := codexLoginCurrent
	codexLoginMu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("no active codex login session")
	}
	if time.Now().After(session.ExpiresAt) {
		CancelCodexLogin()
		return nil, fmt.Errorf("codex login session expired")
	}

	select {
	case code := <-session.codeChan:
		tokens, err := exchangeCodexCode(code, session.Verifier)
		if err != nil {
			CancelCodexLogin()
			return nil, err
		}
		CancelCodexLogin()
		return tokens, nil
	case err := <-session.errChan:
		CancelCodexLogin()
		return nil, err
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("authorization_pending")
	}
}

// CancelCodexLogin tears down any active Codex login session.
func CancelCodexLogin() {
	codexLoginMu.Lock()
	defer codexLoginMu.Unlock()
	if codexLoginCurrent != nil {
		codexLoginCurrent.abort()
		codexLoginCurrent = nil
	}
}

// CurrentCodexLoginURL returns the authorize URL of the active session, or "".
func CurrentCodexLoginURL() string {
	codexLoginMu.Lock()
	defer codexLoginMu.Unlock()
	if codexLoginCurrent == nil {
		return ""
	}
	return codexLoginCurrent.AuthURL
}

// postCodexToken posts URL-encoded form fields to the Codex token endpoint
// and decodes the JSON response.
func postCodexToken(form url.Values) (*CodexTokens, error) {
	req, err := http.NewRequest("POST", codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex token: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token: request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codex token: HTTP %d: %s", resp.StatusCode, truncateForLog(body))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("codex token: parse: %w", err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return nil, fmt.Errorf("codex token: response missing access_token or refresh_token")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 3600
	}

	accountID := extractCodexAccountID(parsed.AccessToken)
	if accountID == "" {
		return nil, fmt.Errorf("codex token: access token JWT missing chatgpt_account_id")
	}

	return &CodexTokens{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    time.Now().Unix() + int64(parsed.ExpiresIn),
		AccountID:    accountID,
	}, nil
}

// exchangeCodexCode trades an authorization code + PKCE verifier for tokens.
func exchangeCodexCode(code, verifier string) (*CodexTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {codexRedirectURI},
	}
	return postCodexToken(form)
}

// RefreshCodexToken refreshes an expired Codex access token using the
// refresh_token grant. Returns new tokens (the old refresh token is rotated).
func RefreshCodexToken(refreshToken string) (*CodexTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("codex refresh: empty refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {codexClientID},
	}
	tokens, err := postCodexToken(form)
	if err != nil {
		return nil, fmt.Errorf("codex refresh: %w", err)
	}
	return tokens, nil
}

// extractCodexAccountID decodes the JWT payload and returns the
// chatgpt_account_id claim, or "" if absent / malformed.
func extractCodexAccountID(accessToken string) string {
	info := extractCodexJWTClaims(accessToken)
	return info.AccountID
}

// CodexJWTInfo holds the profile fields extracted from a Codex access token.
type CodexJWTInfo struct {
	AccountID string // chatgpt_account_id
	PlanType  string // chatgpt_plan_type (free/plus/team/pro)
	Email     string // profile.email
	Name      string // profile.name
}

// extractCodexJWTClaims decodes the JWT payload and returns all useful
// claims: chatgpt_account_id, chatgpt_plan_type, email, name.
func extractCodexJWTClaims(accessToken string) CodexJWTInfo {
	var info CodexJWTInfo
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return info
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return info
		}
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return info
	}
	if authClaim, ok := claims[codexJWTAuthClaim].(map[string]interface{}); ok {
		info.AccountID, _ = authClaim["chatgpt_account_id"].(string)
		info.PlanType, _ = authClaim["chatgpt_plan_type"].(string)
	}
	if profClaim, ok := claims[codexJWTProfileClaim].(map[string]interface{}); ok {
		info.Email, _ = profClaim["email"].(string)
		info.Name, _ = profClaim["name"].(string)
	}
	return info
}

// ExtractCodexJWTInfoPublic is the exported wrapper for callers outside auth.
func ExtractCodexJWTInfoPublic(accessToken string) CodexJWTInfo {
	return extractCodexJWTClaims(accessToken)
}

// truncateForLog trims a response body to a reasonable log length.
func truncateForLog(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// ExtractCodexAccountIDPublic is the exported wrapper around
// extractCodexAccountID for callers outside the auth package (notably
// proxy/external_codex.go, which needs to re-extract the account_id
// after a token refresh without re-implementing JWT decoding).
func ExtractCodexAccountIDPublic(accessToken string) string {
	return extractCodexAccountID(accessToken)
}


