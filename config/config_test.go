package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if got := GetPassword(); got != "new-admin-password" {
		t.Fatalf("expected password to update, got %q", got)
	}
}

func TestUpdateSettingsPatchCanExplicitlyDisableAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected API key to be cleared, got %q", got)
	}
	if IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to be disabled")
	}
	if got := GetPassword(); got != "admin-password" {
		t.Fatalf("expected password to be preserved, got %q", got)
	}
}

func TestSaveAtomicallyReplacesConfigFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "atomic-save", AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := UpdateAccountToken("atomic-save", "access-rotated", "refresh-rotated", 1_800_000_000); err != nil {
		t.Fatalf("update token: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted Config
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("atomic save left invalid JSON: %v", err)
	}
	if len(persisted.Accounts) != 1 || persisted.Accounts[0].RefreshToken != "refresh-rotated" {
		t.Fatalf("rotated credential was not persisted: %+v", persisted.Accounts)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config file mode = %o, want 600", info.Mode().Perm())
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(configPath), ".config.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temporary configs: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary config files were left behind: %v", temps)
	}
}

func TestGetKiroApiTimeout(t *testing.T) {
	prev := os.Getenv("API_TIMEOUT_MS")
	os.Unsetenv("API_TIMEOUT_MS")
	defer os.Setenv("API_TIMEOUT_MS", prev)

	// Test with no config value — should return the Claude-safe default.
	defaultTimeout := GetKiroApiTimeout()
	if defaultTimeout != 15*time.Minute {
		t.Fatalf("expected default 15m, got %v", defaultTimeout)
	}
}

func TestLegacyConfigMigrationAddsClaudeRuntimeDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"accounts":[],"extraModels":["claude-sonnet-5"]}`)
	if err := os.WriteFile(configPath, seed, 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}

	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if got := GetKiroApiTimeout(); got != 15*time.Minute {
		t.Fatalf("migrated API timeout = %s, want 15m", got)
	}
	if got := GetStreamIdleTimeout(); got != 15*time.Minute {
		t.Fatalf("migrated stream idle timeout = %s, want 15m", got)
	}
	if !hasModelID(GetExtraModels(), "claude-fable-5") {
		t.Fatalf("migrated extra models = %v, want claude-fable-5", GetExtraModels())
	}

	persisted, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	var got struct {
		APITimeoutMs             int      `json:"apiTimeoutMs"`
		StreamIdleTimeoutSeconds int      `json:"streamIdleTimeoutSeconds"`
		ExtraModels              []string `json:"extraModels"`
	}
	if err := json.Unmarshal(persisted, &got); err != nil {
		t.Fatalf("decode migrated config: %v", err)
	}
	if got.APITimeoutMs != defaultKiroApiTimeoutMs || got.StreamIdleTimeoutSeconds != defaultStreamIdleTimeoutSecs {
		t.Fatalf("persisted runtime defaults = %+v, want %d/%d", got, defaultKiroApiTimeoutMs, defaultStreamIdleTimeoutSecs)
	}
	if !hasModelID(got.ExtraModels, defaultClaudeExtraModel) {
		t.Fatalf("persisted extra models = %v, want %q", got.ExtraModels, defaultClaudeExtraModel)
	}
}

func TestExplicitDisabledStreamIdleTimeoutIsPreserved(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"accounts":[],"streamIdleTimeoutSeconds":0}`)
	if err := os.WriteFile(configPath, seed, 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}

	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if got := GetStreamIdleTimeout(); got != 0 {
		t.Fatalf("explicit disabled stream idle timeout = %s, want 0", got)
	}
	var persisted map[string]json.RawMessage
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after disabled timeout migration: %v", err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode config after disabled timeout migration: %v", err)
	}
	var value int
	if err := json.Unmarshal(persisted["streamIdleTimeoutSeconds"], &value); err != nil || value != 0 {
		t.Fatalf("persisted disabled stream timeout = %v, want explicit 0", persisted["streamIdleTimeoutSeconds"])
	}
}

func TestSavePreservesNewerRuntimeFieldsFromDisk(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"apiTimeoutMs":300000,"streamIdleTimeoutSeconds":60,"extraModels":["claude-sonnet-5","claude-fable-5"],"accounts":[]}`)
	if err := os.WriteFile(configPath, seed, 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}

	// Simulate another writer updating the runtime policy while this process
	// still holds its original snapshot in memory.
	var disk map[string]json.RawMessage
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	disk["apiTimeoutMs"] = json.RawMessage(`900000`)
	disk["streamIdleTimeoutSeconds"] = json.RawMessage(`900`)
	disk["extraModels"] = json.RawMessage(`["claude-sonnet-5","claude-fable-5","gpt-5.6-sol"]`)
	data, err = json.Marshal(disk)
	if err != nil {
		t.Fatalf("encode updated config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	if err := UpdateStats(1, 1, 0, 42, 0); err != nil {
		t.Fatalf("save stale snapshot: %v", err)
	}
	var persisted struct {
		APITimeoutMs             int      `json:"apiTimeoutMs"`
		StreamIdleTimeoutSeconds int      `json:"streamIdleTimeoutSeconds"`
		ExtraModels              []string `json:"extraModels"`
	}
	persistedData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if persisted.APITimeoutMs != 900000 || persisted.StreamIdleTimeoutSeconds != 900 {
		t.Fatalf("stale save restored runtime timeouts: %+v", persisted)
	}
	if !hasModelID(persisted.ExtraModels, "gpt-5.6-sol") {
		t.Fatalf("stale save restored extra models: %v", persisted.ExtraModels)
	}

	if err := SetExtraModels([]string{"claude-sonnet-5"}); err != nil {
		t.Fatalf("explicit extra model update: %v", err)
	}
	if got := GetExtraModels(); len(got) != 1 || got[0] != "claude-sonnet-5" {
		t.Fatalf("explicit extra model update was merged away: %v", got)
	}
}

func TestCodexPrimaryResetClearsWindowTokensAndRejectsStaleStats(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Now()
	windowMinutes := 7 * 24 * 60
	if err := AddAccount(Account{
		ID:                           "codex-1",
		AuthMethod:                   "codex",
		Enabled:                      true,
		AddedAt:                      now.Add(-8 * 24 * time.Hour).Unix(),
		CodexPrimaryResetAt:          now.Add(time.Hour).Unix(),
		CodexTokensSincePrimaryReset: 250,
		TotalTokens:                  1_000,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	update, err := UpdateAccountCodexUsage(
		"codex-1", "plus", "standard", 0, 0, windowMinutes,
		now.Add(7*24*time.Hour).Unix(), 0, 0, false, false,
	)
	if err != nil {
		t.Fatalf("update Codex usage: %v", err)
	}
	if !update.PrimaryWindowReset {
		t.Fatal("expected a full weekly deadline to start a new primary window")
	}

	// Simulate a queued stats write captured before the upstream reset.
	previousDeadline := now.Add(time.Hour).Unix()
	if err := UpdateAccountStats("codex-1", 4, 0, 1_000, 250, previousDeadline, 0, 1); err != nil {
		t.Fatalf("write stale stats: %v", err)
	}
	accounts := GetAccounts()
	if got := accounts[0].CodexTokensSincePrimaryReset; got != 0 {
		t.Fatalf("stale stats restored previous-window tokens: got %d, want 0", got)
	}

	if err := UpdateAccountStats("codex-1", 5, 0, 1_050, 50, update.PrimaryResetAt, 0, 2); err != nil {
		t.Fatalf("write current-window stats: %v", err)
	}
	accounts = GetAccounts()
	if got := accounts[0].CodexTokensSincePrimaryReset; got != 50 {
		t.Fatalf("current-window stats not persisted: got %d, want 50", got)
	}
}

func TestCodexPrimaryResetDetectedAfterDelayedFirstRequest(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Now()
	window := 7 * 24 * time.Hour
	resetOccurredAt := now.Add(-20 * time.Minute)
	if err := AddAccount(Account{
		ID:                           "codex-delayed-reset",
		AuthMethod:                   "codex",
		Enabled:                      true,
		AddedAt:                      now.Add(-8 * 24 * time.Hour).Unix(),
		TotalTokens:                  1_000,
		CodexTokensSincePrimaryReset: 250,
		CodexPrimaryResetAt:          resetOccurredAt.Unix(),
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	update, err := UpdateAccountCodexUsage(
		"codex-delayed-reset", "plus", "standard", 0, 0, int(window/time.Minute),
		resetOccurredAt.Add(window).Unix(), 0, 0, false, false,
	)
	if err != nil {
		t.Fatalf("update Codex usage: %v", err)
	}
	if !update.PrimaryWindowReset {
		t.Fatalf("expected reset after delayed first request: %+v", update)
	}

	account := GetAccounts()[0]
	if account.CodexTokensSincePrimaryReset != 0 {
		t.Fatalf("delayed reset kept prior-window tokens: got %d, want 0", account.CodexTokensSincePrimaryReset)
	}
}

func TestCodexPrimaryDeadlineJitterDoesNotResetWindowTokens(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Now()
	deadline := now.Add(4 * 24 * time.Hour).Unix()
	if err := AddAccount(Account{
		ID:                           "codex-jitter",
		AuthMethod:                   "codex",
		Enabled:                      true,
		AddedAt:                      now.Add(-time.Hour).Unix(),
		TotalTokens:                  1_000,
		CodexTokensSincePrimaryReset: 250,
		CodexPrimaryResetAt:          deadline,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	update, err := UpdateAccountCodexUsage(
		"codex-jitter", "plus", "premium", 40, 0, 7*24*60,
		deadline+2, 0, 0, false, false,
	)
	if err != nil {
		t.Fatalf("update Codex usage: %v", err)
	}
	if update.PrimaryWindowReset || update.PrimaryWindowChanged {
		t.Fatalf("deadline jitter must not start a new window: %+v", update)
	}

	account := GetAccounts()[0]
	if account.CodexTokensSincePrimaryReset != 250 {
		t.Fatalf("deadline jitter reset window tokens: got %d, want 250", account.CodexTokensSincePrimaryReset)
	}
	if account.CodexPrimaryResetAt != deadline {
		t.Fatalf("deadline jitter changed canonical reset timestamp: got %d, want %d", account.CodexPrimaryResetAt, deadline)
	}
}

func TestCodexWindowCounterBootstrapsFromNewAccountTotal(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Now()
	window := 7 * 24 * time.Hour
	deadline := now.Add(window - time.Minute).Unix()
	if err := AddAccount(Account{
		ID:                           "codex-bootstrap",
		AuthMethod:                   "codex",
		Enabled:                      true,
		AddedAt:                      now.Add(-30 * time.Second).Unix(),
		TotalTokens:                  1_000,
		CodexTokensSincePrimaryReset: 50,
		CodexPrimaryResetAt:          deadline,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	update, err := UpdateAccountCodexUsage(
		"codex-bootstrap", "plus", "premium", 1, 0, 7*24*60,
		deadline+1, 0, 0, false, false,
	)
	if err != nil {
		t.Fatalf("update Codex usage: %v", err)
	}
	if !update.BootstrapCurrentWindowTokens {
		t.Fatalf("expected new account counter bootstrap: %+v", update)
	}

	account := GetAccounts()[0]
	if account.CodexTokensSincePrimaryReset != account.TotalTokens {
		t.Fatalf("bootstrap tokens = %d, total = %d", account.CodexTokensSincePrimaryReset, account.TotalTokens)
	}
	if !account.CodexPrimaryWindowTokensInitialized {
		t.Fatal("expected bootstrap to mark the primary window initialized")
	}
}

func TestCodexWindowCounterDoesNotBackfillAfterRealReset(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Now()
	windowMinutes := 7 * 24 * 60
	if err := AddAccount(Account{
		ID:                           "codex-reset-no-backfill",
		AuthMethod:                   "codex",
		Enabled:                      true,
		AddedAt:                      now.Add(-time.Hour).Unix(),
		TotalTokens:                  1_000,
		CodexTokensSincePrimaryReset: 250,
		CodexPrimaryResetAt:          now.Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	update, err := UpdateAccountCodexUsage(
		"codex-reset-no-backfill", "plus", "premium", 0, 0, windowMinutes,
		now.Add(7*24*time.Hour).Unix(), 0, 0, false, false,
	)
	if err != nil {
		t.Fatalf("record reset: %v", err)
	}
	if !update.PrimaryWindowReset {
		t.Fatal("expected a new primary window")
	}

	// A subsequent header in the same new window must leave the fresh counter
	// at zero until OmniProxy processes new traffic.
	_, err = UpdateAccountCodexUsage(
		"codex-reset-no-backfill", "plus", "premium", 0, 0, windowMinutes,
		now.Add(7*24*time.Hour-time.Minute).Unix(), 0, 0, false, false,
	)
	if err != nil {
		t.Fatalf("record current window: %v", err)
	}
	account := GetAccounts()[0]
	if account.CodexTokensSincePrimaryReset != 0 {
		t.Fatalf("post-reset counter was backfilled: got %d, want 0", account.CodexTokensSincePrimaryReset)
	}
	if !account.CodexPrimaryWindowTokensInitialized {
		t.Fatal("expected reset to mark the primary window initialized")
	}
}

func TestAddAccountSetsAddedAt(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	before := time.Now().Unix()
	if err := AddAccount(Account{ID: "new-account"}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	accounts := GetAccounts()
	if len(accounts) != 1 || accounts[0].AddedAt < before {
		t.Fatalf("expected AddAccount to set addedAt, got %+v", accounts)
	}
}

func TestUpdateAccountPreservesAddedAt(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "account-1", AddedAt: 1_700_000_000}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	if err := UpdateAccount("account-1", Account{ID: "account-1", Nickname: "updated"}); err != nil {
		t.Fatalf("update account: %v", err)
	}
	accounts := GetAccounts()
	if got := accounts[0].AddedAt; got != 1_700_000_000 {
		t.Fatalf("update cleared addedAt: got %d", got)
	}
}

func TestUpdateAccountPreservingCredentialsKeepsLatestTokens(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{
		ID: "credential-account", AccessToken: "access-new", RefreshToken: "refresh-new",
		ExpiresAt: 1_800_000_000, TokenRefreshedAt: 1_700_000_000,
		Enabled: true,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	stale := Account{
		ID: "credential-account", Nickname: "updated metadata",
		AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: 1_600_000_000,
	}
	if err := UpdateAccountPreservingCredentials(stale.ID, stale); err != nil {
		t.Fatalf("preserving update: %v", err)
	}

	account := GetAccounts()[0]
	if account.Nickname != "updated metadata" {
		t.Fatalf("metadata was not updated: %q", account.Nickname)
	}
	if account.AccessToken != "access-new" || account.RefreshToken != "refresh-new" || account.ExpiresAt != 1_800_000_000 {
		t.Fatalf("stale snapshot overwrote credentials: %+v", account)
	}
	if account.TokenRefreshedAt != 1_700_000_000 {
		t.Fatalf("stale snapshot overwrote refresh timestamp: %d", account.TokenRefreshedAt)
	}
}

func TestLegacyCreatedAtMigratesToAddedAt(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"accounts":[{"id":"legacy-account","createdAt":1700000000}]}`)
	if err := os.WriteFile(cfgFile, seed, 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init config: %v", err)
	}
	accounts := GetAccounts()
	if len(accounts) != 1 || accounts[0].AddedAt != 1700000000 {
		t.Fatalf("expected legacy createdAt to migrate to addedAt, got %+v", accounts)
	}

	onDisk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	var persisted struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(onDisk, &persisted); err != nil {
		t.Fatalf("decode migrated config: %v", err)
	}
	if got := persisted.Accounts[0]["addedAt"]; got != float64(1700000000) {
		t.Fatalf("expected persisted addedAt, got %#v", got)
	}
	if _, ok := persisted.Accounts[0]["createdAt"]; ok {
		t.Fatalf("expected legacy createdAt to be removed, got %+v", persisted.Accounts[0])
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}

func TestFindAccountByEmail(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// No accounts yet — should return nil
	if got := FindAccountByEmail("test@example.com"); got != nil {
		t.Fatalf("expected nil for empty config, got %v", got)
	}

	// Add an account with an email
	acc := Account{
		ID:         "acc-1",
		Email:      "test@example.com",
		Enabled:    true,
		AuthMethod: "social",
	}
	if err := AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// Should find it
	found := FindAccountByEmail("test@example.com")
	if found == nil {
		t.Fatal("expected to find account by email")
	}
	if found.ID != "acc-1" {
		t.Fatalf("expected acc-1, got %s", found.ID)
	}

	// Non-existent email returns nil
	if got := FindAccountByEmail("nonexistent@example.com"); got != nil {
		t.Fatalf("expected nil for unknown email, got %v", got)
	}

	// Empty string returns nil
	if got := FindAccountByEmail(""); got != nil {
		t.Fatalf("expected nil for empty email, got %v", got)
	}

	// Case-sensitive match
	if got := FindAccountByEmail("TEST@example.com"); got != nil {
		t.Fatalf("expected nil for different case, got %v", got)
	}
}

// TestFindAccountByProfileArnAndEmail guards the external_idp dedup fix: two
// Azure AD users in the same AWS org share one Q Developer profile ARN, so a new
// login must only match the SAME user (profileArn AND email), never clobber a
// different org member who happens to share the ARN.
func TestFindAccountByProfileArnAndEmail(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const sharedArn = "arn:aws:codewhisperer:us-east-1:111:profile/ORG"
	if err := AddAccount(Account{ID: "alice", Email: "alice@corp.com", ProfileArn: sharedArn, AuthMethod: "external_idp", Enabled: true}); err != nil {
		t.Fatalf("AddAccount alice: %v", err)
	}
	if err := AddAccount(Account{ID: "bob", Email: "bob@corp.com", ProfileArn: sharedArn, AuthMethod: "external_idp", Enabled: true}); err != nil {
		t.Fatalf("AddAccount bob: %v", err)
	}

	// Same ARN + bob's email must return bob, NOT alice (the first ARN match).
	found := FindAccountByProfileArnAndEmail(sharedArn, "bob@corp.com")
	if found == nil || found.ID != "bob" {
		t.Fatalf("expected bob for shared ARN + bob email, got %#v", found)
	}

	// A brand-new org user (same shared ARN, unseen email) must NOT match anyone,
	// so the caller appends instead of overwriting alice/bob.
	if got := FindAccountByProfileArnAndEmail(sharedArn, "carol@corp.com"); got != nil {
		t.Fatalf("expected nil for new org user sharing the ARN, got %q", got.ID)
	}

	// Empty email (unresolved JWT) must never dedup — would otherwise collapse
	// every empty-email account onto the first ARN match.
	if got := FindAccountByProfileArnAndEmail(sharedArn, ""); got != nil {
		t.Fatalf("expected nil for empty email, got %q", got.ID)
	}

	// Empty ARN returns nil regardless of email.
	if got := FindAccountByProfileArnAndEmail("", "alice@corp.com"); got != nil {
		t.Fatalf("expected nil for empty ARN, got %q", got.ID)
	}
}
