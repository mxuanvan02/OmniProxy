package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"testing"
)

// The admin token is a console-level credential: it reads (and in some forks
// can mutate) the account on the gateway's dashboard. It must never travel back
// to a browser through the accounts API — echoing it would place it in browser
// memory, access logs, and any proxy in between, for a page that only needs to
// know whether the integration is configured.
//
// Pinned here because the serializer is a long literal map and a future field
// addition (or a "just include everything" refactor) would leak it silently.
func TestAccountsAPINeverLeaksAdminToken(t *testing.T) {
	const secretToken = "sys-tok-abc123-secret-do-not-leak"
	initConfigForTests(t)

	if err := config.AddAccount(config.Account{
		ID:             "vsllm-leak-test",
		Email:          "vsllm@example.com",
		Nickname:       "VSLLM",
		AuthMethod:     externalAuthMethod,
		Enabled:        true,
		AccessToken:    "sk-inference",
		BaseURL:        "https://vsllm.com/",
		ExtAdminToken:  secretToken,
		ExtAdminUserID: 12639,
		ExtSubscriptions: []config.ExternalSubscription{
			{ID: 196758, Status: "active", UsedPercent: 90, WindowMinutes: 300,
				LastResetTime: 1790611262, NextResetTime: 1790629262},
		},
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	h := &Handler{
		pool:          accountpool.GetPool(),
		catalogStatus: newCatalogStatusStore(),
		usageTracker: &UsageTracker{
			ringCap:    10,
			ring:       make([]RequestRecord, 10),
			activeReqs: make(map[string]ActiveRequest),
			dailyData:  make(map[string]*PeriodSummary),
		},
	}
	h.pool.Reload()

	rec := httptest.NewRecorder()
	h.apiGetAccounts(rec, httptest.NewRequest("GET", "/admin/api/accounts", nil))
	body := rec.Body.String()

	if rec.Code != 200 {
		t.Fatalf("GET /admin/api/accounts = %d, want 200; body=%s", rec.Code, body[:min(len(body), 300)])
	}
	if strings.Contains(body, secretToken) {
		t.Fatal("the admin system token was serialized to the accounts API — a dashboard-level credential must never reach the browser")
	}
	// Positive control: prove the response really does carry this account and
	// its subscription data, so the absence above is not just an empty payload.
	if !strings.Contains(body, "vsllm@example.com") {
		t.Fatal("the account is missing from the response, so the leak check above proved nothing")
	}
	if !strings.Contains(body, `"extAdminConfigured":true`) {
		t.Error(`"extAdminConfigured" should be true so the UI can show the plan-window section`)
	}
	if !strings.Contains(body, "196758") {
		t.Error("the cached subscription row should be serialized for the UI")
	}
}

// The manual-refresh endpoint reports the plan rows but not the credential, and
// it refuses early — without an upstream call — when the account has none.
func TestRefreshSubscriptionsEndpointRefusesWithoutAdminCreds(t *testing.T) {
	initConfigForTests(t)
	if err := config.AddAccount(config.Account{
		ID:          "vsllm-nocreds",
		Email:       "nocreds@example.com",
		AuthMethod:  externalAuthMethod,
		Enabled:     true,
		AccessToken: "sk-only",
		BaseURL:     "https://vsllm.com/",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	h := &Handler{
		pool:          accountpool.GetPool(),
		catalogStatus: newCatalogStatusStore(),
		usageTracker: &UsageTracker{
			ringCap:    10,
			ring:       make([]RequestRecord, 10),
			activeReqs: make(map[string]ActiveRequest),
			dailyData:  make(map[string]*PeriodSummary),
		},
	}
	h.pool.Reload()

	rec := httptest.NewRecorder()
	h.apiRefreshAccountSubscriptions(rec, httptest.NewRequest("POST", "/admin/api/accounts/vsllm-nocreds/subscriptions", nil), "vsllm-nocreds")

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 for an account with no admin credentials", rec.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String()[:min(rec.Body.Len(), 200)])
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("expected an \"error\" key, got %v", out)
	}
}
