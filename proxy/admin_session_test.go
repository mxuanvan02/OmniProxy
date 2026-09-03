package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"testing"
	"time"
)

// issueAdminTestToken mints a live session token for tests that call
// handleAdminAPI directly. The admin password is no longer accepted as a
// credential on those routes, so tests must go through the session store.
func issueAdminTestToken(t *testing.T) string {
	t.Helper()
	token, err := adminSessions.issue()
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	t.Cleanup(func() { adminSessions.revoke(token) })
	return token
}

func TestAdminLoginIssuesTokenAndRejectsWrongPassword(t *testing.T) {
	initConfigForTests(t)
	if err := config.SetPassword("correct-horse-battery"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h := &Handler{}

	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"wrong"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.handleAdminAPI(rec, httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"correct-horse-battery"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if body.Token == "" {
		t.Fatal("login returned no token")
	}
	if body.ExpiresIn != int(adminSessionTTL().Seconds()) {
		t.Fatalf("expiresIn = %d, want %d", body.ExpiresIn, int(adminSessionTTL().Seconds()))
	}
	t.Cleanup(func() { adminSessions.revoke(body.Token) })
	if !adminSessions.valid(body.Token) {
		t.Fatal("issued token is not valid")
	}
}

func TestAdminSessionSurvivesRestartOnLoopback(t *testing.T) {
	initConfigForTests(t)
	store := &adminSessionStore{tokens: make(map[string]time.Time)}
	token, err := store.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	defer store.revoke(token)

	restarted := &adminSessionStore{tokens: make(map[string]time.Time)}
	restarted.load()
	defer restarted.revoke(token)
	if !restarted.valid(token) {
		t.Fatal("persisted local token was not restored")
	}
}

func TestAdminAPIRejectsPasswordAsCredential(t *testing.T) {
	initConfigForTests(t)
	if err := config.SetPassword("correct-horse-battery"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	h := &Handler{}

	for _, tc := range []struct{ name, target, header string }{
		{"query pwd", "/admin/api/status?pwd=correct-horse-battery", ""},
		{"legacy header", "/admin/api/status", "correct-horse-battery"},
		{"query token on non-SSE route", "/admin/api/status?token=correct-horse-battery", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.header != "" {
				req.Header.Set("X-Admin-Password", tc.header)
			}
			rec := httptest.NewRecorder()
			h.handleAdminAPI(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAdminSSERouteAcceptsQueryToken(t *testing.T) {
	token := issueAdminTestToken(t)
	sseURL := "/admin/api/logs/stream?token=" + token
	if got := adminRequestToken(httptest.NewRequest(http.MethodGet, sseURL, nil), "/logs/stream"); got != token {
		t.Fatalf("SSE query token = %q, want %q", got, token)
	}
	statusURL := "/admin/api/status?token=" + token
	if got := adminRequestToken(httptest.NewRequest(http.MethodGet, statusURL, nil), "/status"); got != "" {
		t.Fatalf("non-SSE route accepted query token: %q", got)
	}
}

func TestIsLocalOrSameOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://proxy.internal:8080/admin/api/status", nil)
	r.Host = "proxy.internal:8080"
	for origin, want := range map[string]bool{
		"":                           false,
		"https://evil.example":       false,
		"http://proxy.internal:8080": true,
		"http://localhost:3000":      true,
		"http://127.0.0.1:9999":      true,
		"http://[::1]:9999":          true,
		"http://10.0.0.5:8080":       false,
	} {
		if got := isLocalOrSameOrigin(origin, r); got != want {
			t.Errorf("isLocalOrSameOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}
