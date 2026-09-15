package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExternalAPIDialectSurvivesLoadAndSave covers the anthropic dialect as well
// as responses: both are stored under the same key, and anthropicPath is the
// Messages counterpart of responsesPath.
func TestExternalAPIDialectSurvivesLoadAndSave(t *testing.T) {
	cases := []struct {
		name    string
		seed    string
		dialect string
		pathKey string
		path    string
	}{
		{
			name:    "responses",
			seed:    `"externalApiDialect":"responses","responsesPath":"/codex/responses"`,
			dialect: "responses",
			pathKey: "responsesPath",
			path:    "/codex/responses",
		},
		{
			name:    "anthropic",
			seed:    `"externalApiDialect":"anthropic","anthropicPath":"/anthropic/v1/messages"`,
			dialect: "anthropic",
			pathKey: "anthropicPath",
			path:    "/anthropic/v1/messages",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgFile := filepath.Join(t.TempDir(), "config.json")
			seed := []byte(`{"accounts":[{"id":"dialect-a","enabled":true,` + tc.seed + `}]}`)
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
			if got := accounts[0].ExternalAPIDialect; got != tc.dialect {
				t.Fatalf("loaded ExternalAPIDialect = %q, want %q", got, tc.dialect)
			}
			if got := accountPath(accounts[0], tc.pathKey); got != tc.path {
				t.Fatalf("loaded %s = %q, want %q", tc.pathKey, got, tc.path)
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
			if got := persisted.Accounts[0]["externalApiDialect"]; got != tc.dialect {
				t.Fatalf("persisted externalApiDialect = %v, want %q", got, tc.dialect)
			}
			if got := persisted.Accounts[0][tc.pathKey]; got != tc.path {
				t.Fatalf("persisted %s = %v, want %q", tc.pathKey, got, tc.path)
			}
		})
	}
}

func accountPath(account Account, key string) string {
	if key == "anthropicPath" {
		return account.AnthropicPath
	}
	return account.ResponsesPath
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
	for _, key := range []string{"externalApiDialect", "responsesPath", "anthropicPath"} {
		if _, present := persisted.Accounts[0][key]; present {
			t.Fatalf("%s must be omitted for a chat-dialect account", key)
		}
	}
}
