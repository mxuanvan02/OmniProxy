package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"reflect"
	"strings"
	"testing"
)

func allowedModelsFromAccountJSON(t *testing.T, account config.Account) []string {
	t.Helper()
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	items, ok := payload["allowedModels"].([]interface{})
	if !ok {
		t.Fatalf("allowedModels missing from account JSON: %s", raw)
	}
	models := make([]string, 0, len(items))
	for _, item := range items {
		models = append(models, item.(string))
	}
	return models
}

func TestUpdateAccountEditsAllowedModelsWithoutChangingCredential(t *testing.T) {
	const accountID = "allowlist-account"
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: accountID, AccessToken: "credential-must-survive", Enabled: true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	rec := httptest.NewRecorder()
	body := `{"allowedModels":[" gpt-5.6-terra ","claude-opus-5","gpt-5.6-terra",""]}`
	h.apiUpdateAccount(rec, httptest.NewRequest(http.MethodPut, "/accounts/"+accountID, strings.NewReader(body)), accountID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].AccessToken != "credential-must-survive" {
		t.Fatalf("credential changed during allowlist update")
	}
	want := []string{"gpt-5.6-terra", "claude-opus-5"}
	if got := allowedModelsFromAccountJSON(t, accounts[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowedModels = %v, want normalized %v", got, want)
	}
}

func TestGetAccountsPublishesAllowedModels(t *testing.T) {
	const accountID = "allowlist-visible"
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	raw := []byte(`{"id":"allowlist-visible","enabled":true,"allowedModels":["gpt-5.6-terra"]}`)
	var account config.Account
	if err := json.Unmarshal(raw, &account); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	rec := httptest.NewRecorder()
	h.apiGetAccounts(rec, httptest.NewRequest(http.MethodGet, "/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response) != 1 {
		t.Fatalf("response accounts = %d, want 1", len(response))
	}
	models, ok := response[0]["allowedModels"].([]interface{})
	if !ok || len(models) != 1 || models[0] != "gpt-5.6-terra" {
		t.Fatalf("response allowedModels = %#v", response[0]["allowedModels"])
	}
}
