package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"sync/atomic"
	"testing"
)

func TestBulkRefreshIsolatesAccountFailures(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.Header.Get("Authorization")
		if key == "Bearer broken" || (key == "Bearer partial" && r.URL.Path == "/api/me") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":40100,"message":"token is malformed"}`))
			return
		}
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	for _, id := range []string{"partial", "broken", "healthy"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AuthMethod: externalAuthMethod, AccessToken: id, BaseURL: server.URL}); err != nil {
			t.Fatal(err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiRefreshAllAccounts(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/refresh-all", nil))
	var result struct {
		Refreshed int `json:"refreshed"`
		Partial int `json:"partial"`
		Failed int `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || result.Refreshed != 1 || result.Partial != 1 || result.Failed != 1 {
		t.Fatalf("unexpected bulk result: %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range []string{"partial", "healthy"} {
		if len(p.GetModelList(id)) != 1 {
			t.Errorf("account %s catalog was not refreshed", id)
		}
	}
}

func TestRefreshAccountReportsPartialWhenBillingRejectsToken(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
		case "/api/me":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":40100,"message":"invalid token: token is malformed: token contains an invalid number of segments"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	account := config.Account{ID: "partial-billing", Enabled: true, AuthMethod: externalAuthMethod, AccessToken: "test-key", BaseURL: server.URL}
	if err := config.AddAccount(account); err != nil {
		t.Fatal(err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiRefreshAccount(rec, httptest.NewRequest(http.MethodPost, "/accounts/partial-billing/refresh", nil), account.ID)
	var result struct {
		Success bool `json:"success"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !result.Success || !result.Partial {
		t.Fatalf("expected partial success: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if a := p.GetByID(account.ID); a == nil || !a.Enabled {
		t.Fatal("billing rejection disabled the inference account")
	}
}

// TestRefreshAllCoversExternalAccountsLikePerAccountButton locks in the parity
// fix between the two refresh entry points.
//
// Regression: "Refresh All" called refreshAllAccounts(), the background
// scheduler's conservative pass, which explicitly skips external and service
// accounts and never refreshes model catalogs. Pressing refresh on a single
// external account card called apiRefreshAccount, which DOES refresh the model
// catalog. So the bulk button silently did less work than the per-account
// button with the same icon and a broader label.
//
// Both paths now go through refreshAccountFull, so a bulk refresh must reach an
// external provider's /v1/models endpoint exactly like the single-account path.
func TestRefreshAllCoversExternalAccountsLikePerAccountButton(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	var modelCalls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			atomic.AddInt64(&modelCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
			return
		}
		// Credit endpoint (/api/me) is optional; a 404 must not fail refresh.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	const accountID = "external-refresh-parity"
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Email:       "parity@example.test",
		Nickname:    "PARITY ROUTER",
		AuthMethod:  externalAuthMethod,
		AccessToken: "test-key",
		BaseURL:     server.URL,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("add external account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	// Baseline: the per-account path refreshes the model catalog.
	before := atomic.LoadInt64(&modelCalls)
	var target *config.Account
	for _, a := range config.GetAccounts() {
		if a.ID == accountID {
			acc := a
			target = &acc
			break
		}
	}
	if target == nil {
		t.Fatalf("account %s not found after AddAccount", accountID)
	}
	if _, err := h.refreshAccountFull(target); err != nil {
		t.Fatalf("per-account refresh failed: %v", err)
	}
	perAccountCalls := atomic.LoadInt64(&modelCalls) - before
	if perAccountCalls == 0 {
		t.Fatal("per-account refresh did not fetch the model catalog; test fixture is wrong")
	}

	// The bulk path must do the same work, not skip external accounts.
	beforeBulk := atomic.LoadInt64(&modelCalls)
	outcomes := h.refreshAllAccountsFull()
	bulkCalls := atomic.LoadInt64(&modelCalls) - beforeBulk

	if bulkCalls != perAccountCalls {
		t.Errorf("bulk refresh made %d model-catalog calls, per-account made %d; Refresh All must do the same work",
			bulkCalls, perAccountCalls)
	}

	var found bool
	for _, o := range outcomes {
		if o.AccountID != accountID {
			continue
		}
		found = true
		if o.Skipped {
			t.Error("external account was skipped by Refresh All")
		}
		if o.Err != nil {
			t.Errorf("external account refresh returned error: %v", o.Err)
		}
		if o.Label != "PARITY ROUTER" {
			t.Errorf("outcome label = %q, want the account nickname", o.Label)
		}
	}
	if !found {
		t.Errorf("external account missing from bulk refresh outcomes (%d entries)", len(outcomes))
	}
}

// TestRefreshAllSkipsUnauthenticatedAccounts documents the one case the bulk
// path must NOT attempt: an account that never completed authentication has no
// credential to refresh, so it is reported as skipped rather than failed.
func TestRefreshAllSkipsUnauthenticatedAccounts(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	const accountID = "no-token-account"
	if err := config.AddAccount(config.Account{
		ID:         accountID,
		Email:      "pending@example.test",
		AuthMethod: externalAuthMethod,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	for _, o := range h.refreshAllAccountsFull() {
		if o.AccountID != accountID {
			continue
		}
		if !o.Skipped {
			t.Error("account without an access token must be skipped, not attempted")
		}
		if o.Err != nil {
			t.Errorf("skipped account must not report an error, got %v", o.Err)
		}
		return
	}
	t.Errorf("account %s missing from outcomes", accountID)
}
