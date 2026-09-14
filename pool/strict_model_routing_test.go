package pool

import (
	"omniproxy/config"
	"testing"
)

func TestModelPrefixesShareIdentity(t *testing.T) {
	ids := []string{"claude-opus-5", "anthropic/claude-opus-5", "kr/claude-opus-5"}
	for _, requested := range ids {
		for _, catalog := range ids {
			for _, allowed := range ids {
				p := newModelPool(config.Account{ID: "a", RestrictModels: true, AllowedModels: []string{allowed}})
				p.SetModelList("a", []string{catalog})
				if !p.accountHasModel("a", requested) {
					t.Fatalf("request=%s catalog=%s allowed=%s must share identity", requested, catalog, allowed)
				}
				if p.accountHasModel("a", requested+"-20260801") {
					t.Fatal("dated snapshot must remain a different model")
				}
			}
		}
	}
}

func TestStrictModelEligibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account config.Account
		catalog []string
		want    bool
	}{
		{name: "unknown catalog"},
		{name: "different variant", catalog: []string{"gpt-4o-mini"}},
		{name: "exact model", catalog: []string{"gpt-4o"}, want: true},
		{name: "mapping cannot change model", account: config.Account{ModelMappings: map[string]string{"gpt-4o": "gpt-4o-mini"}}, catalog: []string{"gpt-4o", "gpt-4o-mini"}},
		{name: "identity mapping", account: config.Account{ModelMappings: map[string]string{"gpt-4o": "gpt-4o"}}, catalog: []string{"gpt-4o"}, want: true},
		{name: "permission is not capability", account: config.Account{AllowedModels: []string{"gpt-4o"}}},
		{name: "deny all", account: config.Account{RestrictModels: true}, catalog: []string{"gpt-4o"}},
		{name: "not allowed", account: config.Account{AllowedModels: []string{"gpt-4o-mini"}}, catalog: []string{"gpt-4o"}},
		{name: "allowed and supported", account: config.Account{RestrictModels: true, AllowedModels: []string{"gpt-4o"}}, catalog: []string{"gpt-4o"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.account
			a.ID = "a"
			p := newModelPool(a)
			if tc.catalog != nil {
				p.SetModelList(a.ID, tc.catalog)
			}
			if got := p.accountHasModel(a.ID, "gpt-4o"); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmptyCatalogReplacesPreviousModels(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.SetModelList("a", []string{"gpt-4o"})
	p.SetModelList("a", []string{})
	if p.accountHasModel("a", "gpt-4o") {
		t.Fatal("empty catalog retained stale capability")
	}
}

func TestReloadInvalidatesCatalogWhenConnectionChanges(t *testing.T) {
	initTempPoolConfig(t)
	a := config.Account{ID: "catalog-owner", Enabled: true, AuthMethod: "external_openai", BaseURL: "https://first.example", AccessToken: "test-key"}
	if err := config.AddAccount(a); err != nil {
		t.Fatal(err)
	}
	p := newModelPool()
	p.Reload()
	p.SetModelList(a.ID, []string{"claude-opus-5"})
	a.Nickname = "renamed"
	if err := config.UpdateAccount(a.ID, a); err != nil {
		t.Fatal(err)
	}
	p.Reload()
	if len(p.GetModelList(a.ID)) != 1 {
		t.Fatal("metadata change cleared catalog")
	}
	a.BaseURL = "https://second.example"
	if err := config.UpdateAccount(a.ID, a); err != nil {
		t.Fatal(err)
	}
	p.Reload()
	if p.GetNextForModel("kr/claude-opus-5") != nil {
		t.Fatal("new endpoint inherited old capability")
	}
}
