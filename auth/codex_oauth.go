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
//
//	payload["https://api.openai.com/auth"].chatgpt_account_id
//
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
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Public Codex OAuth constants (extracted from the official Codex CLI binary
// and confirmed by multiple third-party reimplementations). These are public
// values shipped in every Codex CLI install; they are not secrets.
const (
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
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
	ExpiresAt    int64  // Unix seconds
	AccountID    string // chatgpt_account_id extracted from the JWT
}

// CodexLoginSession holds the in-flight state of a Codex PKCE login.
// Only one Codex login can be active at a time because OpenAI registers a
// fixed redirect port (1455); concurrent logins would collide on the port.
type CodexLoginSession struct {
	Verifier  string
	State     string
	AuthURL   string
	StartedAt time.Time
	ExpiresAt time.Time
	// TargetAccountID and ProfileDir are set only when an operator is
	// linking a persistent browser profile to an existing Codex account.
	TargetAccountID string
	ProfileDir      string
	// approvalStatus is used only by the persistent-profile callback page. It
	// remains pending until the admin handler has exchanged the OAuth code and
	// verified that the JWT belongs to TargetAccountID.
	approvalStatus string
	codeChan       chan string
	errChan        chan error
	server         *http.Server

	browserMu  sync.Mutex
	browserCmd *exec.Cmd
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
	return startCodexLogin("", "")
}

// StartCodexLoginForAccount starts the standard PKCE flow in a persistent,
// account-scoped browser profile. The caller must validate the returned JWT
// account ID before treating the profile as linked to TargetAccountID.
func StartCodexLoginForAccount(profileDir, targetAccountID string) (*CodexLoginSession, error) {
	if strings.TrimSpace(profileDir) == "" || strings.TrimSpace(targetAccountID) == "" {
		return nil, fmt.Errorf("codex login: persistent profile and target account are required")
	}
	return startCodexLogin(profileDir, targetAccountID)
}

func startCodexLogin(profileDir, targetAccountID string) (*CodexLoginSession, error) {
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
		Verifier:        verifier,
		State:           state,
		AuthURL:         authURL,
		StartedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(10 * time.Minute),
		TargetAccountID: targetAccountID,
		ProfileDir:      profileDir,
		codeChan:        make(chan string, 1),
		errChan:         make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback/status", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		codexLoginMu.Lock()
		status := "pending"
		if codexLoginCurrent == session && session.approvalStatus != "" {
			status = session.approvalStatus
		}
		codexLoginMu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
	})
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if session.ProfileDir != "" {
			// Do not expose Security until the admin handler exchanges this code
			// and verifies the JWT account ID. The state is a PKCE nonce and is
			// checked again by the local status endpoint.
			stateJSON, _ := json.Marshal(state)
			fmt.Fprintf(w, "<html><body><script>const s=%s;const check=async()=>{try{const r=await fetch('/auth/callback/status?state='+encodeURIComponent(s));const d=await r.json();if(d.status==='approved'){location.replace('https://chatgpt.com/#settings/Security');return}if(d.status==='rejected'){window.close();return}}catch(_){}setTimeout(check,500)};check()</script></body></html>", stateJSON)
		} else {
			fmt.Fprint(w, "<html><body><script>window.close()</script></body></html>")
		}
		select {
		case session.codeChan <- code:
		default:
		}
		// The temporary profile used for an ordinary add-account login can be
		// closed after its callback. A persistent account profile must be left
		// alone so Chrome can flush its authenticated session to disk.
		if session.ProfileDir == "" {
			go func() {
				time.Sleep(500 * time.Millisecond)
				session.closeCleanBrowser()
			}()
		}
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
	s.closeCleanBrowser()
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
		return tokens, nil
	case err := <-session.errChan:
		CancelCodexLogin()
		return nil, err
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("authorization_pending")
	}
}

// CompleteCodexLogin records a successful account check. Persistent-profile
// callback pages observe the approved status and navigate to Security; the
// local callback listener is released shortly afterwards. Temporary logins
// are closed immediately as before.
func CompleteCodexLogin() {
	codexLoginMu.Lock()
	defer codexLoginMu.Unlock()
	if codexLoginCurrent == nil {
		return
	}
	if codexLoginCurrent.ProfileDir != "" {
		session := codexLoginCurrent
		session.approvalStatus = "approved"
		go func() {
			time.Sleep(3 * time.Second)
			codexLoginMu.Lock()
			defer codexLoginMu.Unlock()
			if codexLoginCurrent != session {
				return
			}
			if session.server != nil {
				_ = session.server.Close()
				session.server = nil
			}
			codexLoginCurrent = nil
		}()
		return
	}
	if codexLoginCurrent.server != nil {
		_ = codexLoginCurrent.server.Close()
		codexLoginCurrent.server = nil
	}
	codexLoginCurrent = nil
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

// CurrentCodexLoginTargetAccountID returns the local account being linked by
// the active persistent-profile login, or empty for ordinary add-account logins.
func CurrentCodexLoginTargetAccountID() string {
	codexLoginMu.Lock()
	defer codexLoginMu.Unlock()
	if codexLoginCurrent == nil {
		return ""
	}
	return codexLoginCurrent.TargetAccountID
}

// OpenCodexLoginInCleanBrowser opens the active login in a compact Chrome app.
// Ordinary logins use a temporary profile; account-linking logins use the
// session's persistent profile so later Security actions reopen the same user.
func OpenCodexLoginInCleanBrowser() error {
	codexLoginMu.Lock()
	session := codexLoginCurrent
	codexLoginMu.Unlock()
	if session == nil || session.AuthURL == "" {
		return fmt.Errorf("no active codex login session")
	}

	profileDir := session.ProfileDir
	removeProfileOnExit := false
	if profileDir == "" {
		var err error
		profileDir, err = os.MkdirTemp("", "omniproxy-codex-login-")
		if err != nil {
			return fmt.Errorf("create temporary Chrome profile: %w", err)
		}
		removeProfileOnExit = true
	} else if err := os.MkdirAll(profileDir, 0700); err != nil {
		return fmt.Errorf("create Codex browser profile: %w", err)
	}

	browser, err := findCodexLoginBrowser()
	if err != nil {
		if removeProfileOnExit {
			_ = os.RemoveAll(profileDir)
		}
		return err
	}

	return session.openBrowser(browser, profileDir, session.AuthURL, removeProfileOnExit)
}

// OpenCodexSecurityProfile opens the ChatGPT Security screen in an existing
// account-scoped Chrome profile. It never accepts an arbitrary URL.
func OpenCodexSecurityProfile(profileDir string) error {
	if strings.TrimSpace(profileDir) == "" {
		return fmt.Errorf("Codex browser profile is not configured")
	}
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		return fmt.Errorf("create Codex browser profile: %w", err)
	}
	browser, err := findCodexLoginBrowser()
	if err != nil {
		return err
	}
	cmd := exec.Command(browser,
		"--user-data-dir="+profileDir,
		"--app=https://chatgpt.com/#settings/Security",
		"--window-size=960,760",
		"--disable-background-mode",
		"--no-first-run",
		"--no-default-browser-check",
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Chrome for Codex security settings: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (s *CodexLoginSession) openBrowser(browser, profileDir, pageURL string, removeProfileOnExit bool) error {
	cmd := exec.Command(browser,
		"--user-data-dir="+profileDir,
		"--app="+pageURL,
		"--window-size=480,740",
		"--disable-background-mode",
		"--no-first-run",
		"--no-default-browser-check",
	)

	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.browserCmd != nil && s.browserCmd.Process != nil {
		_ = s.browserCmd.Process.Kill()
	}
	if err := cmd.Start(); err != nil {
		if removeProfileOnExit {
			_ = os.RemoveAll(profileDir)
		}
		return fmt.Errorf("start Chrome for Codex login: %w", err)
	}
	s.browserCmd = cmd

	// Temporary profiles contain throwaway login sessions; persistent profiles
	// deliberately retain cookies for the verified account.
	go func() {
		_ = cmd.Wait()
		if removeProfileOnExit {
			_ = os.RemoveAll(profileDir)
		}
		s.browserMu.Lock()
		if s.browserCmd == cmd {
			s.browserCmd = nil
		}
		s.browserMu.Unlock()
	}()
	return nil
}

func (s *CodexLoginSession) closeCleanBrowser() {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.browserCmd != nil && s.browserCmd.Process != nil {
		_ = s.browserCmd.Process.Kill()
	}
}

func findCodexLoginBrowser() (string, error) {
	if runtime.GOOS == "darwin" {
		const chromeApp = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if info, err := os.Stat(chromeApp); err == nil && !info.IsDir() {
			return chromeApp, nil
		}
	}

	for _, candidate := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Google Chrome was not found; install Chrome to add a Codex account")
}

// postCodexToken posts URL-encoded form fields to the Codex token endpoint
// and decodes the JSON response. A refresh response may omit refresh_token;
// the caller then keeps the currently persisted refresh token.
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
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("codex token: response missing access_token")
	}
	if form.Get("grant_type") != "refresh_token" && parsed.RefreshToken == "" {
		return nil, fmt.Errorf("codex token: authorization response missing refresh_token")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 3600
	}

	accountID := extractCodexAccountID(parsed.AccessToken)
	if accountID == "" {
		return nil, fmt.Errorf("codex token: access token JWT missing chatgpt_account_id")
	}

	expiresAt := time.Now().Unix() + int64(parsed.ExpiresIn)
	if jwtInfo := extractCodexJWTClaims(parsed.AccessToken); jwtInfo.ExpiresAt > 0 {
		expiresAt = jwtInfo.ExpiresAt
	}

	return &CodexTokens{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    expiresAt,
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
	ExpiresAt int64  // exp, when present (Unix seconds)
}

// extractCodexJWTClaims decodes the JWT payload and returns all useful
// claims: chatgpt_account_id, chatgpt_plan_type, email, name, exp.
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
	// JSON numbers decode as float64. Only accept a positive, integral
	// expiration; malformed/missing exp must not make callers discard a token.
	if exp, ok := claims["exp"].(float64); ok && exp > 0 && exp == float64(int64(exp)) {
		info.ExpiresAt = int64(exp)
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
