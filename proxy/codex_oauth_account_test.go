package proxy

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omniproxy/auth"
	"omniproxy/config"
)

func TestUpsertCodexOAuthAccountReusesExistingIdentity(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:               "existing-codex",
		AuthMethod:       codexAuthMethod,
		ChatGPTAccountID: "acct_same",
		AccessToken:      "access-old",
		RefreshToken:     "refresh-old",
		Nickname:         "Operator label",
		MachineId:        "stable-machine-id",
		Weight:           3,
		Enabled:          false,
		BanStatus:        "BANNED",
	}); err != nil {
		t.Fatalf("add existing account: %v", err)
	}

	updated, created, err := upsertCodexOAuthAccount(config.Account{
		ID:               "new-random-id",
		AuthMethod:       codexAuthMethod,
		ChatGPTAccountID: "acct_same",
		AccessToken:      "access-new",
		RefreshToken:     "refresh-new",
		ExpiresAt:        1_900_000_000,
		Email:            "user@example.test",
		Nickname:         "JWT name",
		Provider:         "OpenAI Codex",
		Region:           "external",
		Enabled:          true,
		MachineId:        "new-machine-id",
		CodexEmail:       "user@example.test",
		CodexName:        "JWT name",
		CodexPlanType:    "plus",
	}, false)
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if created {
		t.Fatal("same ChatGPT account created a duplicate")
	}
	if updated.ID != "existing-codex" || updated.MachineId != "stable-machine-id" || updated.Weight != 3 {
		t.Fatalf("upsert did not preserve local identity/settings: %+v", updated)
	}
	if updated.Nickname != "Operator label" {
		t.Fatalf("implicit OAuth login replaced operator nickname: %q", updated.Nickname)
	}
	if updated.AccessToken != "access-new" || updated.RefreshToken != "refresh-new" || !updated.Enabled || updated.BanStatus != "ACTIVE" {
		t.Fatalf("upsert did not activate replacement credentials: %+v", updated)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected exactly one persisted Codex account, got %d", len(accounts))
	}
	if _, created, err := upsertCodexOAuthAccount(updated, true); err != nil || created {
		t.Fatalf("explicit-name upsert unexpectedly created/failed: created=%v err=%v", created, err)
	}
}

func TestCodexBrowserProfileDirIsAccountScopedAndOpaque(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	first := codexBrowserProfileDir("account/../one")
	second := codexBrowserProfileDir("account/../two")
	if first == second {
		t.Fatal("different account IDs must have different browser profile paths")
	}
	if strings.Contains(first, "account") || strings.Contains(first, "..") {
		t.Fatalf("profile path leaks account ID into filesystem path: %q", first)
	}
	if filepath.Base(first) != strings.Repeat("0", 64) && len(filepath.Base(first)) != 64 {
		t.Fatalf("profile directory name must be a SHA-256 hex digest, got %q", filepath.Base(first))
	}
}

func TestAdminCodexSecurityRejectsUnknownAndNonCodexAccounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	h := &Handler{}

	unknown := httptest.NewRecorder()
	h.apiOpenCodexSecurity(unknown, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/missing/codex-security", nil), "missing")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want %d", unknown.Code, http.StatusNotFound)
	}

	if err := config.AddAccount(config.Account{ID: "not-codex", AuthMethod: "social", Enabled: true}); err != nil {
		t.Fatalf("add non-Codex account: %v", err)
	}
	nonCodex := httptest.NewRecorder()
	h.apiOpenCodexSecurity(nonCodex, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/not-codex/codex-security", nil), "not-codex")
	if nonCodex.Code != http.StatusBadRequest {
		t.Fatalf("non-Codex account status = %d, want %d", nonCodex.Code, http.StatusBadRequest)
	}
}

func TestQuarantineCodexBrowserProfilePreservesExistingData(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	profileDir := codexBrowserProfileDir("codex-account")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	marker := filepath.Join(profileDir, "session-marker")
	if err := os.WriteFile(marker, []byte("preserve me"), 0600); err != nil {
		t.Fatalf("write profile marker: %v", err)
	}

	archived, err := quarantineCodexBrowserProfile(profileDir)
	if err != nil {
		t.Fatalf("quarantine profile: %v", err)
	}
	if archived == "" || archived == profileDir {
		t.Fatalf("unexpected archived path: %q", archived)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("active profile remains after quarantine: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(archived, "session-marker"))
	if err != nil || string(data) != "preserve me" {
		t.Fatalf("archived profile data was not preserved: data=%q err=%v", data, err)
	}
}

func TestCodexProfileMismatchPreservesCredentialsAndArchivesProfile(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const localID = "codex-profile-target"
	if err := config.AddAccount(config.Account{
		ID:               localID,
		AuthMethod:       codexAuthMethod,
		ChatGPTAccountID: "acct_expected",
		AccessToken:      "access-old",
		RefreshToken:     "refresh-old",
		Enabled:          true,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	profileDir := codexBrowserProfileDir(localID)
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "session-marker"), []byte("keep"), 0600); err != nil {
		t.Fatalf("write profile marker: %v", err)
	}

	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"access_token":"` + testCodexJWT("acct_wrong") + `","refresh_token":"refresh-wrong","expires_in":3600}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})})
	defer auth.SetGlobalAuthClientForTest(previousClient)

	session, err := auth.StartCodexLoginForAccount(profileDir, localID)
	if err != nil {
		if strings.Contains(err.Error(), "callback server") {
			t.Skipf("Codex callback port unavailable: %v", err)
		}
		t.Fatalf("start linked login: %v", err)
	}
	defer auth.CancelCodexLogin()

	authURL, err := url.Parse(session.AuthURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	state := authURL.Query().Get("state")
	callbackURL := "http://127.0.0.1:1455/auth/callback?state=" + url.QueryEscape(state) + "&code=local-test-code"
	callbackClient := &http.Client{Transport: &http.Transport{Proxy: nil}}
	response, err := callbackClient.Get(callbackURL)
	if err != nil {
		t.Fatalf("send OAuth callback: %v", err)
	}
	_ = response.Body.Close()

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiCodexLoginPoll(rec, httptest.NewRequest(http.MethodPost, "/admin/api/auth/codex/poll", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched login status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].AccessToken != "access-old" || accounts[0].RefreshToken != "refresh-old" {
		t.Fatalf("mismatched login changed persisted credentials: %+v", accounts)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("active mismatched profile should be archived: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(filepath.Dir(profileDir), "unverified", filepath.Base(profileDir)+"-*", "session-marker"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("mismatched profile was not archived: archives=%v err=%v", archives, err)
	}
}

func testCodexJWT(accountID string) string {
	payload, _ := json.Marshal(map[string]interface{}{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
