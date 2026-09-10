package pool

import (
	"encoding/json"
	"omniproxy/config"
	"testing"
)

func accountWithAllowedModels(t *testing.T, id, authMethod string, allowedModels []string) config.Account {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"id":            id,
		"authMethod":    authMethod,
		"allowedModels": allowedModels,
	})
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	var account config.Account
	if err := json.Unmarshal(raw, &account); err != nil {
		t.Fatalf("unmarshal account: %v", err)
	}
	return account
}

func TestGetNextForModelSkipsPreferredAccountOutsideItsAllowlist(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(
		accountWithAllowedModels(t, "external-restricted", "external_openai", []string{"gpt-5.6-terra"}),
		accountWithAllowedModels(t, "native-fallback", "", nil),
	)
	p.SetModelList("external-restricted", []string{"claude-opus-5", "gpt-5.6-terra"})
	p.SetModelList("native-fallback", []string{"claude-opus-5"})

	for i := 0; i < 4; i++ {
		got := p.GetNextForModel("claude-opus-5")
		if got == nil || got.ID != "native-fallback" {
			t.Fatalf("iteration %d selected %#v, want allowlisted fallback", i, got)
		}
	}
}

func TestGetNextForModelTreatsEmptyAllowlistAsUnrestricted(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(accountWithAllowedModels(t, "legacy", "", []string{}))
	p.SetModelList("legacy", []string{"claude-opus-5"})

	if got := p.GetNextForModel("claude-opus-5"); got == nil || got.ID != "legacy" {
		t.Fatalf("empty allowlist account was restricted: %#v", got)
	}
}
