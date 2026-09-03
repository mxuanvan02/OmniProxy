package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"omniproxy/auth"
	"omniproxy/config"
	accountpool "omniproxy/pool"
)

func TestEnsureValidTokenRefreshesExpiredCodexJWTWithStalePersistedExpiry(t *testing.T) {
	initConfigForTests(t)
	auth.ResetRotationMapForTest()

	const accountID = "codex-expired-jwt"
	const oldRefreshToken = "refresh-old"
	const newRefreshToken = "refresh-rotated"
	now := time.Now().Unix()
	oldAccessToken := testCodexJWTWithExpiry("acct-expired-jwt", now-600)
	newAccessToken := testCodexJWTWithExpiry("acct-expired-jwt", now+3600)

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse OAuth refresh form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := r.Form.Get("refresh_token"); got != oldRefreshToken {
			t.Fatalf("refresh_token = %q, want original token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600}`,
			newAccessToken, newRefreshToken)
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse mock OAuth URL: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		return transport.RoundTrip(clone)
	})})
	t.Cleanup(func() {
		auth.SetGlobalAuthClientForTest(previousClient)
		transport.CloseIdleConnections()
	})

	if err := config.AddAccount(config.Account{
		ID:               accountID,
		Email:            "expired@example.test",
		AuthMethod:       codexAuthMethod,
		AccessToken:      oldAccessToken,
		RefreshToken:     oldRefreshToken,
		ChatGPTAccountID: "acct-expired-jwt",
		// This intentionally conflicts with the expired JWT. The JWT must win.
		ExpiresAt: now + 24*60*60,
		Enabled:   true,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	account := p.GetByID(accountID)
	if account == nil {
		t.Fatal("account missing from pool")
	}
	if err := h.ensureValidToken(account); err != nil {
		t.Fatalf("ensureValidToken: %v", err)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("OAuth refresh calls = %d, want 1", got)
	}
	if account.AccessToken != newAccessToken || account.RefreshToken != newRefreshToken {
		t.Fatalf("in-memory account was not rotated: %#v", account)
	}

	persisted := config.GetAccounts()
	if len(persisted) != 1 || persisted[0].AccessToken != newAccessToken || persisted[0].RefreshToken != newRefreshToken {
		t.Fatalf("config did not persist rotated Codex token: %#v", persisted)
	}
	pooled := p.GetByID(accountID)
	if pooled == nil || pooled.AccessToken != newAccessToken || pooled.RefreshToken != newRefreshToken {
		t.Fatalf("pool did not receive rotated Codex token: %#v", pooled)
	}
}

func testCodexJWTWithExpiry(accountID string, expiresAt int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]interface{}{
		"exp": expiresAt,
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		panic(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
}
