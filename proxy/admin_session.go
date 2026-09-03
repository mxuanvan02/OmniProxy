package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"omniproxy/config"
	"omniproxy/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	adminTokenTTL      = 12 * time.Hour
	adminTokenLocalTTL = 30 * 24 * time.Hour
	adminTokenMax      = 64
	adminTokenHeader   = "X-Admin-Token"
	adminSessionsFile  = "admin_sessions.json"
)

// adminSessionStore holds short-lived bearer tokens handed out by
// /admin/api/login. The admin password itself never travels again after login,
// so it cannot leak through a URL, a referrer, or a proxy access log.
type adminSessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token → expiry
}

var adminSessions = &adminSessionStore{tokens: make(map[string]time.Time)}

// adminSecretEqual compares two secrets in constant time. Both sides are hashed
// first so the comparison length is fixed and the secret's length does not leak.
func adminSecretEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *adminSessionStore) issue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, t)
		}
	}
	// Cap the map so a login loop cannot grow it without bound; drop the entry
	// closest to expiry first.
	for len(s.tokens) >= adminTokenMax {
		var oldest string
		var oldestExp time.Time
		for t, exp := range s.tokens {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = t, exp
			}
		}
		delete(s.tokens, oldest)
	}
	s.tokens[token] = now.Add(adminSessionTTL())
	if err := s.saveLocked(); err != nil {
		delete(s.tokens, token)
		return "", err
	}
	return token, nil
}

// valid reports whether token is live, comparing against every entry so the
// answer does not depend on how much of the token matched.
func (s *adminSessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	ok := false
	for t, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, t)
			changed = true
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	if changed {
		if err := s.saveLocked(); err != nil {
			logger.Warnf("[AdminSession] persist cleanup failed: %v", err)
		}
	}
	return ok
}

func (s *adminSessionStore) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[token]; !ok {
		return
	}
	delete(s.tokens, token)
	if err := s.saveLocked(); err != nil {
		logger.Warnf("[AdminSession] persist logout failed: %v", err)
	}
}

// revokeAll drops every live session. Called when the admin password changes:
// rotating the password after a suspected compromise must not leave the old
// holder's token valid for the rest of its TTL.
func (s *adminSessionStore) revokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = make(map[string]time.Time)
	if err := s.saveLocked(); err != nil {
		logger.Warnf("[AdminSession] persist revocation failed: %v", err)
	}
}

func adminSessionTTL() time.Duration {
	host := strings.TrimSpace(strings.ToLower(config.GetHost()))
	if host == "localhost" {
		return adminTokenLocalTTL
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return adminTokenLocalTTL
	}
	return adminTokenTTL
}

func adminSessionsPath() string {
	return filepath.Join(config.GetConfigDir(), adminSessionsFile)
}

func (s *adminSessionStore) load() {
	if adminSessionTTL() != adminTokenLocalTTL {
		return
	}
	data, err := os.ReadFile(adminSessionsPath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("[AdminSession] load failed: %v", err)
		}
		return
	}
	var tokens map[string]time.Time
	if err := json.Unmarshal(data, &tokens); err != nil {
		logger.Warnf("[AdminSession] invalid session store: %v", err)
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = make(map[string]time.Time, len(tokens))
	for token, expiry := range tokens {
		if token != "" && expiry.After(now) {
			s.tokens[token] = expiry
		}
	}
	if err := s.saveLocked(); err != nil {
		logger.Warnf("[AdminSession] persist loaded sessions failed: %v", err)
	}
}

func (s *adminSessionStore) saveLocked() error {
	if adminSessionTTL() != adminTokenLocalTTL {
		return nil
	}
	data, err := json.Marshal(s.tokens)
	if err != nil {
		return err
	}
	dir := config.GetConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+adminSessionsFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, adminSessionsPath())
}

// adminSSEPaths are the only admin routes that accept the token as a query
// parameter: EventSource cannot set request headers.
var adminSSEPaths = map[string]bool{
	"/logs/stream":  true,
	"/usage/stream": true,
}

// adminRequestToken extracts the session token from the header, or from the
// query string for the two SSE routes.
func adminRequestToken(r *http.Request, path string) string {
	if tok := r.Header.Get(adminTokenHeader); tok != "" {
		return tok
	}
	if adminSSEPaths[path] {
		return r.URL.Query().Get("token")
	}
	return ""
}

// apiAdminLogin POST /admin/api/login {"password":"…"} → {"token":"…"}.
func (h *Handler) apiAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	stored := config.GetPassword()
	if stored == "" || !adminSecretEqual(req.Password, stored) {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	token, err := adminSessions.issue()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not create session"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":     token,
		"expiresIn": int(adminSessionTTL().Seconds()),
	})
}

// apiAdminLogout POST /admin/api/logout revokes the caller's own token.
func (h *Handler) apiAdminLogout(w http.ResponseWriter, r *http.Request) {
	adminSessions.revoke(r.Header.Get(adminTokenHeader))
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// isLocalOrSameOrigin reports whether origin may read admin API responses:
// same host as the request, or a loopback address. Everything else gets no CORS
// header at all, so a page the operator visits cannot read stored OAuth tokens.
func isLocalOrSameOrigin(origin string, r *http.Request) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
