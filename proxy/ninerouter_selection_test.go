package proxy

import (
	"omniproxy/auth"
	"omniproxy/config"
	"testing"
)

func TestNineRouterSelectionIncludes(t *testing.T) {
	cases := []struct {
		name      string
		account   auth.NineRouterImportedAccount
		index     int
		sourceIDs []string
		indexes   []int
		want      bool
	}{
		{
			name:    "legacy request imports every record",
			account: auth.NineRouterImportedAccount{SourceID: "source-1"},
			index:   3,
			want:    true,
		},
		{
			name:      "source id is selected",
			account:   auth.NineRouterImportedAccount{SourceID: "source-1"},
			sourceIDs: []string{" source-1 "},
			indexes:   []int{99},
			want:      true,
		},
		{
			name:    "source id does not use index fallback",
			account: auth.NineRouterImportedAccount{SourceID: "source-1"},
			index:   2,
			indexes: []int{2},
			want:    false,
		},
		{
			name:    "record without source id uses selected index",
			account: auth.NineRouterImportedAccount{},
			index:   2,
			indexes: []int{1, 2},
			want:    true,
		},
		{
			name:      "empty explicit selection imports nothing",
			account:   auth.NineRouterImportedAccount{SourceID: "source-1"},
			want:      false,
			sourceIDs: []string{},
			indexes:   []int{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nineRouterSelectionIncludes(tc.account, tc.index, tc.sourceIDs, tc.indexes); got != tc.want {
				t.Fatalf("nineRouterSelectionIncludes() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNineRouterAccountsToJSONIncludesSelectionMetadataWithoutSecrets(t *testing.T) {
	accounts := nineRouterAccountsToJSON([]auth.NineRouterImportedAccount{{
		SourceID:     "source-1",
		Provider:     "tavily",
		Name:         "Search",
		AccessToken:  "secret-token",
		Capabilities: []string{"search"},
	}})

	if len(accounts) != 1 {
		t.Fatalf("preview account count = %d, want 1", len(accounts))
	}
	entry := accounts[0]
	if entry["index"] != 0 || entry["sourceId"] != "source-1" {
		t.Fatalf("selection metadata = %#v", entry)
	}
	if _, ok := entry["accessToken"]; ok {
		t.Fatal("preview must not expose accessToken")
	}
	if _, ok := entry["apiKey"]; ok {
		t.Fatal("preview must not expose apiKey")
	}
}

func TestImportOneNineRouterGenericUpdatesRotatedRefreshToken(t *testing.T) {
	initConfigForTests(t)

	existing := config.Account{
		ID:           "provider-account",
		Email:        "tavily-9router",
		Nickname:     "Old name",
		AuthMethod:   "service_api_key",
		Provider:     "tavily",
		SourceID:     "source-1",
		AccessToken:  "old-key",
		RefreshToken: "old-refresh",
		ProviderKind: "search",
		Capabilities: []string{"search"},
		Enabled:      true,
	}
	if err := config.AddAccount(existing); err != nil {
		t.Fatalf("add existing provider: %v", err)
	}

	h := &Handler{}
	got, err := h.importOne9RouterGeneric(auth.NineRouterImportedAccount{
		SourceID:     "source-1",
		Provider:     "tavily",
		Name:         "Updated name",
		APIKey:       "new-key",
		RefreshToken: "new-refresh",
		ProviderKind: "search",
		Capabilities: []string{"search"},
	})
	if err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if got == nil || got.ID != existing.ID {
		t.Fatalf("updated account = %#v, want existing account", got)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	updated := accounts[0]
	if updated.AccessToken != "new-key" || updated.RefreshToken != "new-refresh" {
		t.Fatalf("credentials were not rotated: access=%q refresh=%q", updated.AccessToken, updated.RefreshToken)
	}
	if updated.Nickname != "Updated name" {
		t.Fatalf("nickname = %q, want updated name", updated.Nickname)
	}
}
