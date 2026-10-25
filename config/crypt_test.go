package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Seed one account with every sensitive field set, through the public API so
// the normal save path runs.
func seedAccountWithSecrets(t *testing.T, cfgPath string) {
	t.Helper()
	if err := Init(cfgPath); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "acct-secret",
		Email:        "secret@example.com",
		Enabled:      true,
		AccessToken:  "at-plain-secret-value",
		RefreshToken: "rt-plain-secret-value",
		ClientSecret: "cs-plain-secret-value",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
}

// The on-disk config must not carry plaintext credentials. The same config
// reloaded into a fresh process must still hand out plaintext tokens in
// memory, because every request path and the admin API read them there.
func TestSaveEncryptsTokensOnDiskAndReloadRecoversThem(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	seedAccountWithSecrets(t, cfgPath)

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	disk := string(raw)
	for _, secret := range []string{"at-plain-secret-value", "rt-plain-secret-value", "cs-plain-secret-value"} {
		if strings.Contains(disk, secret) {
			t.Fatalf("config file contains plaintext secret %q", secret)
		}
	}
	if !strings.Contains(disk, "enc:v1:") {
		t.Fatalf("config file has no encrypted values; disk = %s", disk)
	}

	// A fresh load (simulating a restart) must recover the plaintext values.
	if err := Init(cfgPath); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	var found *Account
	for i := range GetAccounts() {
		if GetAccounts()[i].ID == "acct-secret" {
			found = &GetAccounts()[i]
		}
	}
	if found == nil {
		t.Fatal("account missing after reload")
	}
	if found.AccessToken != "at-plain-secret-value" ||
		found.RefreshToken != "rt-plain-secret-value" ||
		found.ClientSecret != "cs-plain-secret-value" {
		t.Fatalf("reloaded secrets wrong: at=%q rt=%q cs=%q",
			found.AccessToken, found.RefreshToken, found.ClientSecret)
	}
}

// Configs written before encryption existed carry plaintext tokens. They must
// load unchanged; the values get encrypted on the next save.
func TestLoadReadsLegacyPlaintextConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
	  "accounts": [
	    {"id": "legacy-1", "email": "legacy@example.com", "enabled": true,
	     "accessToken": "legacy-at-plain", "refreshToken": "legacy-rt-plain"}
	  ]
	}`
	if err := os.WriteFile(cfgPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(cfgPath); err != nil {
		t.Fatalf("Init legacy: %v", err)
	}
	accs := GetAccounts()
	if len(accs) != 1 || accs[0].AccessToken != "legacy-at-plain" {
		t.Fatalf("legacy accounts = %+v, want plaintext token readable", accs)
	}
}

// The key file must exist with 0600 permissions; a world-readable key defeats
// the whole mechanism.
func TestKeyFileIsCreatedWithRestrictedPermissions(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	seedAccountWithSecrets(t, cfgPath)

	keyPath := filepath.Join(filepath.Dir(cfgPath), "config.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
}

// The ciphertext must not be deterministic across saves: two encryptions of
// the same token must differ (fresh nonce), otherwise the file leaks equality
// of credentials across accounts.
func TestEncryptionIsNonDeterministic(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	seedAccountWithSecrets(t, cfgPath)
	first, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Touch the config so a second save happens, then compare.
	if err := SetPublishDiscoveredModels(true); err != nil {
		t.Fatalf("trigger second save: %v", err)
	}
	t.Cleanup(func() { _ = SetPublishDiscoveredModels(false) })
	second, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var f, s map[string]json.RawMessage
	if err := json.Unmarshal(first, &f); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &s); err != nil {
		t.Fatal(err)
	}
	var fa, sa []map[string]json.RawMessage
	if err := json.Unmarshal(f["accounts"], &fa); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(s["accounts"], &sa); err != nil {
		t.Fatal(err)
	}
	var fTok, sTok string
	if err := json.Unmarshal(fa[0]["accessToken"], &fTok); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sa[0]["accessToken"], &sTok); err != nil {
		t.Fatal(err)
	}
	if fTok == sTok {
		t.Fatalf("two saves produced identical ciphertext %q — nonce reuse", fTok)
	}
}
