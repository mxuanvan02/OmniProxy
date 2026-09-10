package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func accountAllowedModelsFromJSON(t *testing.T, account Account) []string {
	t.Helper()
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode account JSON: %v", err)
	}
	items, ok := payload["allowedModels"].([]interface{})
	if !ok {
		t.Fatalf("allowedModels missing from account JSON: %s", raw)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("allowedModels contains non-string value %#v", item)
		}
		result = append(result, text)
	}
	return result
}

func TestAccountAllowedModelsSurviveLoadAndSave(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"accounts":[{"id":"restricted","enabled":true,"allowedModels":["gpt-5.6-terra","claude-opus-5"]}]}`)
	if err := os.WriteFile(cfgFile, seed, 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}

	accounts := GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	want := []string{"gpt-5.6-terra", "claude-opus-5"}
	if got := accountAllowedModelsFromJSON(t, accounts[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded allowedModels = %v, want %v", got, want)
	}
	if err := UpdateAccountPreservingCredentials("restricted", accounts[0]); err != nil {
		t.Fatalf("save account: %v", err)
	}

	onDisk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(onDisk, &persisted); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	if len(persisted.Accounts) != 1 {
		t.Fatalf("persisted accounts = %d, want 1", len(persisted.Accounts))
	}
	if got, ok := persisted.Accounts[0]["allowedModels"].([]interface{}); !ok || len(got) != 2 {
		t.Fatalf("persisted allowedModels = %#v, want two models", persisted.Accounts[0]["allowedModels"])
	}
}
