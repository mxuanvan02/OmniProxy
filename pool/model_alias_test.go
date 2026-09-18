package pool

import (
	"omniproxy/config"
	"testing"
	"time"
)

func TestCanonicalModelKeyGroupsDeploymentVariants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare stays bare", "qwen3.8-max", "qwen3.8-max"},
		{"locale cn", "qwen3.8-max-cn", "qwen3.8-max"},
		{"locale on", "deepseek-v4-flash-on", "deepseek-v4-flash"},
		{"dated snapshot", "deepseek-v4-pro-0813", "deepseek-v4-pro"},
		{"case folded", "Qwen3.8-Max-CN", "qwen3.8-max"},
		{"provider prefix stripped", "apiforcode/glm-5.3-cn", "glm-5.3"},
		{"locale plus snapshot", "deepseek-v4-flash-cn-0731", "deepseek-v4-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalModelKey(tc.in); got != tc.want {
				t.Errorf("canonicalModelKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalModelKeyKeepsBehaviourSuffixes(t *testing.T) {
	// Behaviour-changing variants are distinct models: the key must NOT
	// collapse them into the bare family, or routing would silently swap a
	// thinking/agent/flash model for the base one.
	behavioural := []string{
		"qwen3.8-max-agent",
		"qwen3.8-max-fast-agent",
		"qwen3.8-max-thinking-agent",
		"qwen3.8-flash",
		"gemini-3.6-flash-high",
		"deepseek-v4-flash-vision-exp",
	}
	for _, id := range behavioural {
		if got := canonicalModelKey(id); got != id {
			t.Errorf("canonicalModelKey(%q) = %q, want unchanged", id, got)
		}
	}
}

func TestRankAliasCandidatesOrdering(t *testing.T) {
	key := "qwen3.8-max"
	names := []string{
		"qwen3.8-max-cn",
		"qwen3.8-max-0901",
		"qwen3.8-max",
		"qwen3.8-max-on",
		"qwen3.8-max-0813",
	}
	got := rankAliasCandidates("qwen3.8-max", key, names)
	want := []string{
		"qwen3.8-max",    // exact
		"qwen3.8-max-cn", // locale, configured order
		"qwen3.8-max-on",
		"qwen3.8-max-0901", // snapshot, newest first
		"qwen3.8-max-0813",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestRankAliasCandidatesExactMissingFallsToBare(t *testing.T) {
	key := "glm-5.3"
	got := rankAliasCandidates("glm-5.3", key, []string{"glm-5.3-cn", "glm-5.3"})
	if len(got) == 0 || got[0] != "glm-5.3" {
		t.Fatalf("bare family must lead when exact is absent: %v", got)
	}
}

func TestFindAvailableAliasModelNilPoolAndBlankRequest(t *testing.T) {
	var p *AccountPool
	if got := p.FindAvailableAliasModel("qwen3.8-max"); got != "" {
		t.Fatalf("nil pool returned %q, want empty", got)
	}
	p = &AccountPool{}
	if got := p.FindAvailableAliasModel("   "); got != "" {
		t.Fatalf("blank request returned %q, want empty", got)
	}
}

// The rescue path must return the deployment variant a healthy account can
// actually serve, and skip variants whose only account is cooling down.
func TestFindAvailableAliasModelPrefersHealthyVariant(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "cn-only", AuthMethod: "external_openai"},
		config.Account{ID: "on-only", AuthMethod: "external_openai"},
	)
	p.SetModelList("cn-only", []string{"qwen3.8-max-cn"})
	p.SetModelList("on-only", []string{"qwen3.8-max-on"})

	got := p.FindAvailableAliasModel("qwen3.8-max")
	if got != "qwen3.8-max-cn" {
		t.Fatalf("alias = %q, want the configured-first locale qwen3.8-max-cn", got)
	}

	// Cooling the -cn account must hand the request to -on instead of failing.
	p.cooldowns["cn-only"] = time.Now().Add(time.Minute)
	if got := p.FindAvailableAliasModel("qwen3.8-max"); got != "qwen3.8-max-on" {
		t.Fatalf("alias after cooldown = %q, want qwen3.8-max-on", got)
	}

	// With every variant cooling, the rescue must decline rather than offer a
	// candidate the pool would reject anyway.
	p.cooldowns["on-only"] = time.Now().Add(time.Minute)
	if got := p.FindAvailableAliasModel("qwen3.8-max"); got != "" {
		t.Fatalf("alias with all accounts cooling = %q, want empty", got)
	}
}

// A variant family with no other member must not be invented: an unrelated
// model sharing a prefix (qwen3.8-max-agent) is a different model, not an
// alias of qwen3.8-max.
func TestFindAvailableAliasModelIgnoresBehaviourVariants(t *testing.T) {
	p := newModelPool(config.Account{ID: "agent-only", AuthMethod: "external_openai"})
	p.SetModelList("agent-only", []string{"qwen3.8-max-agent", "qwen3.8-max-thinking-agent"})

	if got := p.FindAvailableAliasModel("qwen3.8-max"); got != "" {
		t.Fatalf("alias = %q, want empty: agent variants are distinct models", got)
	}
}

// The cross-family rescue must return the first fallback an eligible account
// can serve, crossing model families where FindAvailableAliasModel stops. A
// nil pool or an empty list must decline rather than invent a candidate.
func TestFindAvailableFallbackModelNilPoolAndEmptyList(t *testing.T) {
	var p *AccountPool
	if got := p.FindAvailableFallbackModel([]string{"qwen3.8-max"}); got != "" {
		t.Fatalf("nil pool returned %q, want empty", got)
	}
	p = newModelPool(config.Account{ID: "acc", AuthMethod: "external_openai"})
	p.SetModelList("acc", []string{"qwen3.8-max"})
	if got := p.FindAvailableFallbackModel(nil); got != "" {
		t.Fatalf("empty fallback list returned %q, want empty", got)
	}
}

// A request the pool never served (a Claude name on a Qwen-only pool) has no
// same-family variant, so the cross-family list is the only rescue. The first
// servable entry wins, an unavailable earlier entry is skipped, and the
// returned ID is the normalized catalog name the upstream gateway serves.
func TestFindAvailableFallbackModelPicksFirstServable(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "qwen", AuthMethod: "external_openai"},
	)
	p.SetModelList("qwen", []string{"qwen3.8-max", "glm-5.3"})

	// Exact claude-opus-5 is not in any catalog, so the family rescue declines.
	if got := p.FindAvailableAliasModel("claude-opus-5"); got != "" {
		t.Fatalf("alias = %q, want empty: claude family has no pooled member", got)
	}
	// The first fallback is unhealthy (no account serves it), the second wins.
	got := p.FindAvailableFallbackModel([]string{"deepseek-chat", "qwen3.8-max", "glm-5.3"})
	if got != "qwen3.8-max" {
		t.Fatalf("fallback = %q, want qwen3.8-max", got)
	}
}

// A fallback whose only account is cooling down must be skipped so the rescue
// never offers a model the pool would then reject.
func TestFindAvailableFallbackModelSkipsCoolingAccount(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "qwen", AuthMethod: "external_openai"},
		config.Account{ID: "glm", AuthMethod: "external_openai"},
	)
	p.SetModelList("qwen", []string{"qwen3.8-max"})
	p.SetModelList("glm", []string{"glm-5.3"})

	p.cooldowns["qwen"] = time.Now().Add(time.Minute)
	got := p.FindAvailableFallbackModel([]string{"qwen3.8-max", "glm-5.3"})
	if got != "glm-5.3" {
		t.Fatalf("fallback = %q, want glm-5.3 after qwen cools down", got)
	}

	p.cooldowns["glm"] = time.Now().Add(time.Minute)
	if got := p.FindAvailableFallbackModel([]string{"qwen3.8-max", "glm-5.3"}); got != "" {
		t.Fatalf("fallback = %q, want empty when every fallback cools down", got)
	}
}
