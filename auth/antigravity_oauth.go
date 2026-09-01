// Package auth: antigravity_oauth.go
//
// Google Antigravity (Cloud Code Assist) OAuth 2.0 + PKCE login flow.
//
// Antigravity ships a public desktop OAuth client. Its ID and secret are not
// secrets in the OAuth sense (a desktop client cannot keep one) and PKCE is
// what actually protects the exchange, but they are not committed here either:
// supply them through ANTIGRAVITY_CLIENT_ID / ANTIGRAVITY_CLIENT_SECRET or the
// matching config settings, using the values the installed IDE already holds.
//
//   - Authorize: https://accounts.google.com/o/oauth2/auth
//   - Token:     https://oauth2.googleapis.com/token
//   - Redirect:  http://localhost:51121/oauth-callback (loopback, any port)
//
// Google's Antigravity Terms of Service prohibit third-party clients. Accounts
// used through this path can be disabled by Google; that is an expected
// outcome, not an edge case. OmniProxy deliberately sends the protocol headers
// Antigravity's own API requires and nothing more: no rotating platform or
// api-client values, no synthetic telemetry, no attempt to look like a
// different client than it is.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"omniproxy/config"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	antigravityAuthorizeURL = "https://accounts.google.com/o/oauth2/auth"
	antigravityTokenURL     = "https://oauth2.googleapis.com/token"
	antigravityCallbackPath = "/oauth-callback"

	// antigravityPreferredPort matches the IDE's registered loopback port.
	// Google allows any loopback port for desktop clients, so a busy port
	// falls back to an ephemeral one instead of failing the login.
	antigravityPreferredPort = 51121
)

// antigravityScopes are the scopes the Cloud Code Assist API requires.
var antigravityScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// antigravityClientCreds resolves the desktop client's ID and secret from the
// environment first, then the stored settings. Neither is committed: the values
// belong to Google's IDE, and a login that cannot find them should fail with a
// clear message instead of sending an empty client_id to Google.
func antigravityClientCreds() (string, string, error) {
	id := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CLIENT_SECRET"))
	if id == "" {
		id = strings.TrimSpace(config.GetStringSetting("antigravityClientId", ""))
	}
	if secret == "" {
		secret = strings.TrimSpace(config.GetStringSetting("antigravityClientSecret", ""))
	}
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("antigravity: no OAuth client configured (set ANTIGRAVITY_CLIENT_ID and ANTIGRAVITY_CLIENT_SECRET, or the antigravityClientId/antigravityClientSecret settings)")
	}
	return id, secret, nil
}

// AntigravityTokens is the result of a successful exchange or refresh.
type AntigravityTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix seconds
	Email        string
	Name         string
	Subject      string // Google account "sub" claim, stable per account
}

// AntigravityLoginSession holds in-flight PKCE login state. Only one login can
// be active at a time because the callback binds a fixed local port first.
type AntigravityLoginSession struct {
	Verifier    string
	State       string
	AuthURL     string
	RedirectURI string
	ExpiresAt   time.Time

	codeChan chan string
	errChan  chan error
	server   *http.Server
}

var (
	antigravityLoginMu      sync.Mutex
	antigravityLoginCurrent *AntigravityLoginSession
)

// StartAntigravityLogin begins a PKCE login flow and starts the loopback
// callback server. The caller must open AuthURL and then poll.
func StartAntigravityLogin() (*AntigravityLoginSession, error) {
	antigravityLoginMu.Lock()
	defer antigravityLoginMu.Unlock()

	if antigravityLoginCurrent != nil {
		antigravityLoginCurrent.abort()
		antigravityLoginCurrent = nil
	}

	clientID, _, err := antigravityClientCreds()
	if err != nil {
		return nil, err
	}

	listener, err := listenLoopback(antigravityPreferredPort)
	if err != nil {
		return nil, fmt.Errorf("antigravity login: bind loopback callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	verifier := generateCodeVerifier()
	session := &AntigravityLoginSession{
		Verifier:    verifier,
		State:       generateAntigravityState(),
		RedirectURI: fmt.Sprintf("http://localhost:%d%s", port, antigravityCallbackPath),
		ExpiresAt:   time.Now().Add(10 * time.Minute),
		codeChan:    make(chan string, 1),
		errChan:     make(chan error, 1),
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", session.RedirectURI)
	q.Set("scope", strings.Join(antigravityScopes, " "))
	q.Set("code_challenge", generateCodeChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", session.State)
	// offline + consent are required to receive a refresh_token every time;
	// without them Google omits it for an account that already granted access.
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	session.AuthURL = antigravityAuthorizeURL + "?" + q.Encode()

	mux := http.NewServeMux()
	mux.HandleFunc(antigravityCallbackPath, session.handleCallback)
	session.server = &http.Server{Handler: mux, IdleTimeout: 30 * time.Second}
	go func() { _ = session.server.Serve(listener) }()

	antigravityLoginCurrent = session
	return session, nil
}

// listenLoopback prefers the IDE's registered port and falls back to an
// ephemeral one when it is already in use (e.g. Antigravity itself is running).
func listenLoopback(preferred int) (net.Listener, error) {
	if listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred)); err == nil {
		return listener, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func (s *AntigravityLoginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("state") != s.State {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	if errStr := q.Get("error"); errStr != "" {
		select {
		case s.errChan <- fmt.Errorf("oauth error: %s: %s", errStr, q.Get("error_description")):
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<html><body>Antigravity account linked. You can close this tab.<script>window.close()</script></body></html>")
	select {
	case s.codeChan <- code:
	default:
	}
}

func (s *AntigravityLoginSession) abort() {
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

// PollAntigravityLogin waits briefly for the callback and exchanges the code.
// It returns an "authorization_pending" error while the user is still in the
// browser, mirroring the Codex login contract.
func PollAntigravityLogin() (*AntigravityTokens, error) {
	antigravityLoginMu.Lock()
	session := antigravityLoginCurrent
	antigravityLoginMu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("no active antigravity login session")
	}
	if time.Now().After(session.ExpiresAt) {
		CancelAntigravityLogin()
		return nil, fmt.Errorf("antigravity login session expired")
	}

	select {
	case code := <-session.codeChan:
		tokens, err := exchangeAntigravityCode(code, session.Verifier, session.RedirectURI)
		CancelAntigravityLogin()
		if err != nil {
			return nil, err
		}
		return tokens, nil
	case err := <-session.errChan:
		CancelAntigravityLogin()
		return nil, err
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("authorization_pending")
	}
}

// CancelAntigravityLogin tears down any active login session.
func CancelAntigravityLogin() {
	antigravityLoginMu.Lock()
	defer antigravityLoginMu.Unlock()
	if antigravityLoginCurrent != nil {
		antigravityLoginCurrent.abort()
		antigravityLoginCurrent = nil
	}
}

// CurrentAntigravityLoginURL returns the authorize URL of the active session.
func CurrentAntigravityLoginURL() string {
	antigravityLoginMu.Lock()
	defer antigravityLoginMu.Unlock()
	if antigravityLoginCurrent == nil {
		return ""
	}
	return antigravityLoginCurrent.AuthURL
}

func generateAntigravityState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func exchangeAntigravityCode(code, verifier, redirectURI string) (*AntigravityTokens, error) {
	clientID, clientSecret, err := antigravityClientCreds()
	if err != nil {
		return nil, err
	}
	return postAntigravityToken(url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	})
}

// RefreshAntigravityToken exchanges a refresh token for a new access token.
// Google does not rotate the refresh token here, so an empty RefreshToken in
// the result means "keep the stored one".
func RefreshAntigravityToken(refreshToken string) (*AntigravityTokens, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("antigravity refresh: empty refresh token")
	}
	clientID, clientSecret, err := antigravityClientCreds()
	if err != nil {
		return nil, err
	}
	tokens, err := postAntigravityToken(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return nil, fmt.Errorf("antigravity refresh: %w", err)
	}
	return tokens, nil
}

func postAntigravityToken(form url.Values) (*AntigravityTokens, error) {
	req, err := http.NewRequest("POST", antigravityTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("antigravity token: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("antigravity token: request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("antigravity token: HTTP %d: %s", resp.StatusCode, truncateForLog(body))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("antigravity token: parse: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("antigravity token: response missing access_token")
	}
	if form.Get("grant_type") == "authorization_code" && parsed.RefreshToken == "" {
		return nil, fmt.Errorf("antigravity token: authorization response missing refresh_token")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 3600
	}

	info := DecodeGoogleIDToken(parsed.IDToken)
	return &AntigravityTokens{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    time.Now().Unix() + int64(parsed.ExpiresIn),
		Email:        info.Email,
		Name:         info.Name,
		Subject:      info.Subject,
	}, nil
}

// GoogleIDTokenInfo holds the identity claims OmniProxy displays for an account.
type GoogleIDTokenInfo struct {
	Email   string
	Name    string
	Subject string
}

// DecodeGoogleIDToken reads the identity claims from an ID token without
// verifying its signature. The token arrives directly from Google's token
// endpoint over TLS, so it is used for display only, never for authorization.
func DecodeGoogleIDToken(idToken string) GoogleIDTokenInfo {
	var info GoogleIDTokenInfo
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return info
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return info
		}
	}
	var claims struct {
		Email   string `json:"email"`
		Name    string `json:"name"`
		Subject string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return info
	}
	info.Email = claims.Email
	info.Name = claims.Name
	info.Subject = claims.Subject
	return info
}

// FetchGoogleUserInfo resolves the identity behind an access token via
// Google's userinfo endpoint. It is the fallback for imported credentials that
// carry no id_token, where the account's email would otherwise be unknown and
// two different Google accounts could not be told apart.
func FetchGoogleUserInfo(accessToken string) GoogleIDTokenInfo {
	var info GoogleIDTokenInfo
	if strings.TrimSpace(accessToken) == "" {
		return info
	}
	req, err := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return info
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return info
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return info
	}
	var claims struct {
		Email   string `json:"email"`
		Name    string `json:"name"`
		Subject string `json:"sub"`
	}
	if json.Unmarshal(body, &claims) != nil {
		return info
	}
	info.Email = claims.Email
	info.Name = claims.Name
	info.Subject = claims.Subject
	return info
}

// LocalAntigravityCreds is a credential set read from an installed Antigravity
// or Gemini CLI on this machine.
type LocalAntigravityCreds struct {
	Path         string `json:"path"`
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	ExpiresAt    int64  `json:"expiresAt"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	Subject      string `json:"-"`
	ProjectID    string `json:"projectId,omitempty"`
}

// antigravityCredsCandidates lists the credential files written by Google's own
// clients, most specific first.
func antigravityCredsCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".gemini", "antigravity", "oauth_creds.json"),
		filepath.Join(home, ".antigravity", "oauth_creds.json"),
		filepath.Join(home, ".gemini", "oauth_creds.json"),
	}
}

// ReadLocalAntigravityCreds imports credentials from an installed client. It
// exists so an operator can move an account they already authorised in the IDE
// instead of running a second OAuth grant for the same Google account.
func ReadLocalAntigravityCreds() (*LocalAntigravityCreds, error) {
	var lastErr error
	for _, path := range antigravityCredsCandidates() {
		creds, err := readAntigravityCredsFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				lastErr = err
			}
			continue
		}
		creds.ProjectID = readLocalAntigravityProjectID()
		return creds, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no Antigravity credentials found (looked in ~/.gemini and ~/.antigravity)")
}

func readAntigravityCredsFile(path string) (*LocalAntigravityCreds, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		// google-auth-library writes expiry_date in milliseconds; other
		// clients write expiry as an RFC3339 string.
		ExpiryDate int64  `json:"expiry_date"`
		Expiry     string `json:"expiry"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if strings.TrimSpace(parsed.RefreshToken) == "" {
		return nil, fmt.Errorf("%s has no refresh_token", filepath.Base(path))
	}
	creds := &LocalAntigravityCreds{
		Path:         path,
		AccessToken:  strings.TrimSpace(parsed.AccessToken),
		RefreshToken: strings.TrimSpace(parsed.RefreshToken),
	}
	switch {
	case parsed.ExpiryDate > 0:
		creds.ExpiresAt = parsed.ExpiryDate / 1000
	case parsed.Expiry != "":
		if t, err := time.Parse(time.RFC3339, parsed.Expiry); err == nil {
			creds.ExpiresAt = t.Unix()
		}
	}
	info := DecodeGoogleIDToken(parsed.IDToken)
	creds.Email, creds.Name, creds.Subject = info.Email, info.Name, info.Subject
	return creds, nil
}

// readLocalAntigravityProjectID reads the GCP project the local client already
// resolved, so an import can skip the loadCodeAssist round-trip.
func readLocalAntigravityProjectID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "projects.json"))
	if err != nil {
		return ""
	}
	var byPath map[string]struct {
		ProjectID string `json:"projectId"`
		Project   string `json:"project"`
	}
	if json.Unmarshal(data, &byPath) != nil {
		return ""
	}
	for _, entry := range byPath {
		if id := strings.TrimSpace(entry.ProjectID); id != "" {
			return id
		}
		if id := strings.TrimSpace(entry.Project); id != "" {
			return id
		}
	}
	return ""
}
