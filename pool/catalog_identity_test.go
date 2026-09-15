package pool

import (
	"path/filepath"
	"testing"

	"omniproxy/config"
)

// TestUpdateTokenKeepsCatalogIdentityInSync pins the invariant that a routine
// credential rotation is not treated as a new connection.
//
// catalogIdentities is built from the account record at Reload() and compared
// again in SetModelListForAccount. UpdateToken writes a rotated access token
// straight into p.accounts, so unless it updates catalogIdentities too, every
// later model discovery for that account is rejected as
// "account changed during model discovery" and the account stays with an empty
// catalog until the next Reload.
func TestUpdateTokenKeepsCatalogIdentityInSync(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "ag",
		Enabled:      true,
		AuthMethod:   "antigravity",
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		Capabilities: []string{"chat"},
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	p.UpdateToken("ag", "access-new", "refresh-new", 12345)

	rotated := p.GetByID("ag")
	if rotated == nil {
		t.Fatal("GetByID returned nil after UpdateToken")
	}
	if rotated.AccessToken != "access-new" {
		t.Fatalf("AccessToken = %q, want access-new", rotated.AccessToken)
	}
	if !p.SetModelListForAccount(*rotated, []string{"gemini-3-pro-high"}) {
		t.Fatal("SetModelListForAccount rejected the account after a routine token rotation")
	}
	if got := p.GetModelList("ag"); len(got) != 1 || got[0] != "gemini-3-pro-high" {
		t.Fatalf("GetModelList = %v, want [gemini-3-pro-high]", got)
	}
}

// TestSetModelListForAccountRejectsSwappedCredential is the other half of the
// guard: a genuine credential swap must still be refused, so syncing the
// identity on rotation must not turn the check into a no-op.
func TestSetModelListForAccountRejectsSwappedCredential(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "ag",
		Enabled:      true,
		AuthMethod:   "antigravity",
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		Capabilities: []string{"chat"},
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	// A different account's credentials reach discovery through the stale copy.
	stale := config.Account{ID: "ag", AuthMethod: "antigravity", AccessToken: "someone-else", RefreshToken: "someone-else"}
	if p.SetModelListForAccount(stale, []string{"gemini-3-pro-high"}) {
		t.Fatal("SetModelListForAccount accepted discovery completed for a different credential")
	}
	if got := p.GetModelList("ag"); len(got) != 0 {
		t.Fatalf("GetModelList = %v, want empty", got)
	}
}
