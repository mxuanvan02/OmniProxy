package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omniproxy/config"
	accountpool "omniproxy/pool"
)

// antigravityAdminAccount stores an Antigravity account pointed at a stub
// upstream and returns the account ID.
func antigravityAdminAccount(t *testing.T, id, upstreamURL string) *Handler {
	t.Helper()
	initConfigForTests(t)
	if err := config.AddAccount(config.Account{
		ID:                          id,
		Email:                       "owner@example.com",
		AuthMethod:                  "antigravity",
		Provider:                    antigravityProviderLabel,
		Enabled:                     true,
		BaseURL:                     upstreamURL,
		AccessToken:                 "token",
		ExpiresAt:                   time.Now().Add(time.Hour).Unix(),
		GoogleProjectID:             "aicode-consumers",
		AntigravityProjectCheckedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p, catalogStatus: newCatalogStatusStore()}
}

// TestAccountModelsRoutesAntigravityToItsOwnCatalog pins the routing: an
// Antigravity account has no Kiro credential, so the fall-through to
// ListAvailableModels sent its Google access token to q.external.amazonaws.com
// and the dashboard showed a DNS error. The account must be answered from
// fetchAvailableModels on its own endpoint instead.
func TestAccountModelsRoutesAntigravityToItsOwnCatalog(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":{
			"models/gemini-3-pro-high":{"displayName":"Gemini 3 Pro (High)","maxTokens":1048576,"maxOutputTokens":65536},
			"models/claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6","maxTokens":200000,"maxOutputTokens":64000}
		}}`)
	}))
	defer upstream.Close()

	const id = "ag-admin-models"
	h := antigravityAdminAccount(t, id, upstream.URL)

	rec := httptest.NewRecorder()
	h.apiGetAccountModels(rec, httptest.NewRequest(http.MethodGet, "/accounts/"+id+"/models", nil), id)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != antigravityModelsAction {
		t.Errorf("upstream path = %q, want %q — the request did not go to the Antigravity endpoint", gotPath, antigravityModelsAction)
	}

	var body struct {
		Success bool        `json:"success"`
		Models  []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if !body.Success {
		t.Fatalf("success = false, body = %s", rec.Body.String())
	}
	if len(body.Models) != 2 {
		t.Fatalf("models = %d, want 2: %s", len(body.Models), rec.Body.String())
	}

	// The routing cache has to learn the same list, or the account stays
	// unroutable however healthy the response looked.
	routed := h.pool.GetModelList(id)
	if len(routed) != 2 {
		t.Fatalf("pool model list = %v, want the two discovered models", routed)
	}
	if status, ok := h.catalogStatus.get(id); !ok || status.State != CatalogStateVerified {
		t.Errorf("catalog status = %+v (present %v), want state %q", status, ok, CatalogStateVerified)
	}
}

// TestAccountModelsAntigravityFallsBackWhenUpstreamFails keeps the account
// routable: fetchAvailableModels is not available in every environment, and an
// empty catalog leaves the dashboard with nothing to run and the model picker
// blank.
func TestAccountModelsAntigravityFallsBackWhenUpstreamFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"not found"}}`)
	}))
	defer upstream.Close()

	const id = "ag-admin-fallback"
	h := antigravityAdminAccount(t, id, upstream.URL)

	rec := httptest.NewRecorder()
	h.apiGetAccountModels(rec, httptest.NewRequest(http.MethodGet, "/accounts/"+id+"/models", nil), id)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("body = %s, want a success envelope", rec.Body.String())
	}
	if got := len(h.pool.GetModelList(id)); got == 0 {
		t.Fatalf("pool model list is empty; the static fallback did not reach routing")
	}
	if status, _ := h.catalogStatus.get(id); status.State != CatalogStateStatic {
		t.Errorf("catalog state = %q, want %q so the dashboard can say the list was not confirmed",
			status.State, CatalogStateStatic)
	}
}
