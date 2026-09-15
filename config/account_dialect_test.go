package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The admin UI posts externalApiDialect / responsesPath and the add-key handler
// writes them straight onto the account literal, so the JSON tag names are the
// entire contract between the two. A renamed tag would silently drop the
// operator's choice, which is why this test asserts on the on-disk key rather
// than only on the struct field.
func TestExternalAPIDialectSurvivesLoadAndSave(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"accounts":[{"id":"dialect-a","enabled":true,` +
		`"externalApiDialect":"responses","responsesPath":"/codex/responses"}]}`)
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
	if got := accounts[0].ExternalAPIDialect; got != "responses" {
		t.Fatalf("loaded ExternalAPIDialect = %q, want %q", got, "responses")
	}
	if got := accounts[0].ResponsesPath; got != "/codex/responses" {
		t.Fatalf("loaded ResponsesPath = %q, want %q", got, "/codex/responses")
	}
	if err := UpdateAccountPreservingCredentials("dialect-a", accounts[0]); err != nil {
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
	if got := persisted.Accounts[0]["externalApiDialect"]; got != "responses" {
		t.Fatalf("persisted externalApiDialect = %v, want %q", got, "responses")
	}
	if got := persisted.Accounts[0]["responsesPath"]; got != "/codex/responses" {
		t.Fatalf("persisted responsesPath = %v, want %q", got, "/codex/responses")
	}
}

// Every external account that existed before this feature has neither key. The
// omitempty tags are what keep those stored records byte-identical after an
// unrelated edit, so a stray zero-value write must fail here rather than quietly
// rewrite the config of accounts the operator never touched.
func TestChatDialectAccountsOmitBothKeys(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"accounts":[{"id":"chat-a","enabled":true}]}`), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	accounts := GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if err := UpdateAccountPreservingCredentials("chat-a", accounts[0]); err != nil {
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
	for _, key := range []string{"externalApiDialect", "responsesPath"} {
		if _, present := persisted.Accounts[0][key]; present {
			t.Fatalf("%s must be omitted for a chat-dialect account", key)
		}
	}
}
