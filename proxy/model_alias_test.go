package proxy

import (
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"path/filepath"
	"testing"
)

// newAliasTestPool seeds a single-account pool serving the given catalog and
// returns a Handler wired to it. The account is external/chat-capable so Reload
// admits it into the routable set. Callers set ModelFallbacks separately.
func newAliasTestPool(t *testing.T, accountID string, models ...string) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: accountID, Enabled: true, AuthMethod: externalAuthMethod, AccessToken: accountID,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList(accountID, models)
	return &Handler{pool: p}
}

// The happy path must not pay for any rescue: when an account serves the exact
// model, resolveModelAlias returns it unchanged and never reads the fallbacks.
func TestResolveModelAliasExactAvailableUnchanged(t *testing.T) {
	h := newAliasTestPool(t, "qwen", "qwen3.8-max")
	if err := config.SetModelFallbacks([]string{"glm-5.3"}); err != nil {
		t.Fatalf("SetModelFallbacks: %v", err)
	}
	if got := h.resolveModelAlias("qwen3.8-max"); got != "qwen3.8-max" {
		t.Fatalf("resolved = %q, want qwen3.8-max unchanged", got)
	}
}

// A same-family deploy variant outranks the cross-family list: qwen3.8-max is
// unavailable but qwen3.8-max-cn is served, so the alias wins and the fallback
// (a different family) is never consulted.
func TestResolveModelAliasPrefersSameFamilyOverFallback(t *testing.T) {
	h := newAliasTestPool(t, "cn", "qwen3.8-max-cn")
	if err := config.SetModelFallbacks([]string{"glm-5.3"}); err != nil {
		t.Fatalf("SetModelFallbacks: %v", err)
	}
	if got := h.resolveModelAlias("qwen3.8-max"); got != "qwen3.8-max-cn" {
		t.Fatalf("resolved = %q, want the same-family alias qwen3.8-max-cn", got)
	}
}

// The reported bug: a Claude name advertised on a Qwen-only pool has no
// same-family variant, so without the cross-family rescue it 503s with
// "No available accounts". With fallbacks configured it must route to the
// pool's main model instead of failing.
func TestResolveModelAliasCrossFamilyFallback(t *testing.T) {
	h := newAliasTestPool(t, "qwen", "qwen3.8-max")
	if err := config.SetModelFallbacks([]string{"qwen3.8-max"}); err != nil {
		t.Fatalf("SetModelFallbacks: %v", err)
	}
	if got := h.resolveModelAlias("claude-opus-5"); got != "qwen3.8-max" {
		t.Fatalf("resolved = %q, want qwen3.8-max", got)
	}
}

// An empty fallback list is the default install and must preserve the historic
// behaviour: the model is returned unchanged so the caller still reports 503
// rather than silently serving a different model.
func TestResolveModelAliasNoFallbackKeepsModel(t *testing.T) {
	h := newAliasTestPool(t, "qwen", "qwen3.8-max")
	if err := config.SetModelFallbacks(nil); err != nil {
		t.Fatalf("SetModelFallbacks: %v", err)
	}
	if got := h.resolveModelAlias("claude-opus-5"); got != "claude-opus-5" {
		t.Fatalf("resolved = %q, want claude-opus-5 unchanged", got)
	}
}
