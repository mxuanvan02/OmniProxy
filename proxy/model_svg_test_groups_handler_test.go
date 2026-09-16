package proxy

import (
	"strings"
	"testing"

	"omniproxy/config"
)

func TestServableModels(t *testing.T) {
	catalog := []string{"GPT-4o", "deepseek-v4-flash", "Kimi-K3"}

	// Unrestricted: the whole catalog, lowercased and sorted.
	unrestricted := &config.Account{ID: "a1"}
	if got := servableModels(unrestricted, catalog); strings.Join(got, ",") != "deepseek-v4-flash,gpt-4o,kimi-k3" {
		t.Errorf("unrestricted = %v, want lowercased sorted catalog", got)
	}

	// An allowlist restricts even without the RestrictModels flag, matching the
	// pool's `RestrictModels || len(AllowedModels) > 0` gate.
	allowOnly := &config.Account{ID: "a2", AllowedModels: []string{"gpt-4o"}}
	if got := servableModels(allowOnly, catalog); strings.Join(got, ",") != "gpt-4o" {
		t.Errorf("allowlist-only = %v, want [gpt-4o]", got)
	}

	// RestrictModels with an allowlist keeps allowed models the catalog missed.
	restricted := &config.Account{ID: "a3", RestrictModels: true, AllowedModels: []string{"gpt-4o", "gpt-6-astra"}}
	if got := servableModels(restricted, catalog); strings.Join(got, ",") != "gpt-4o,gpt-6-astra" {
		t.Errorf("restricted = %v, want [gpt-4o gpt-6-astra]", got)
	}

	// RestrictModels with an empty allowlist denies everything.
	denyAll := &config.Account{ID: "a4", RestrictModels: true}
	if got := servableModels(denyAll, catalog); len(got) != 0 {
		t.Errorf("restrict-all = %v, want empty", got)
	}

	// An empty catalog yields nothing to offer.
	if got := servableModels(unrestricted, nil); len(got) != 0 {
		t.Errorf("empty catalog = %v, want empty", got)
	}
}
