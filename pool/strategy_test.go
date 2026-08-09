package pool

import (
	"omniproxy/config"
	"path/filepath"
	"testing"
	"time"
)

// strategyTestSetup initialises a temp config so KVSettings helpers work.
// Called at the start of each strategy test that reads/writes KVSettings.
func strategyTestSetup(t *testing.T) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

// TestStrategyRoundRobinIsDefault verifies the default strategy is round-robin
// and that strategyShouldApply returns false for it (no overhead).
func TestStrategyRoundRobinIsDefault(t *testing.T) {
	strategyTestSetup(t)
	// Ensure no KVSetting is set (default path).
	config.SetStringSetting("poolRoutingStrategy", "")
	if got := poolRoutingStrategy(); got != "round-robin" {
		t.Fatalf("default strategy = %q, want round-robin", got)
	}

	p := &AccountPool{}
	// 25 accounts — above the threshold, but round-robin never applies.
	accs := make([]config.Account, 25)
	for i := range accs {
		accs[i] = config.Account{ID: string(rune('a' + i))}
	}
	p.accounts = accs
	if p.strategyShouldApply() {
		t.Fatalf("round-robin should never trigger strategyShouldApply")
	}
}

// TestStrategyCostOptimizedPrefersLowUsage verifies cost-optimized picks
// the account with the lowest CodexPrimaryUsedPercent.
func TestStrategyCostOptimizedPrefersLowUsage(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "cost-optimized")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	accs := []config.Account{
		{ID: "high", AuthMethod: "codex", CodexPrimaryUsedPercent: 90},
		{ID: "mid", AuthMethod: "codex", CodexPrimaryUsedPercent: 50},
		{ID: "low", AuthMethod: "codex", CodexPrimaryUsedPercent: 10},
	}
	picked := pickByStrategy(accs, time.Now())
	if picked == nil {
		t.Fatalf("expected non-nil pick")
	}
	if picked.ID != "low" {
		t.Fatalf("cost-optimized picked %q, want low (10%% used)", picked.ID)
	}
}

// TestStrategyCostOptimizedExternalPrefersMoreRemaining verifies external
// accounts are ranked by remaining credits (more = preferred).
func TestStrategyCostOptimizedExternalPrefersMoreRemaining(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "cost-optimized")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	accs := []config.Account{
		{ID: "less", AuthMethod: "external_openai", ExtCreditLimit: 100, ExtCreditsRemaining: 10},
		{ID: "more", AuthMethod: "external_openai", ExtCreditLimit: 100, ExtCreditsRemaining: 80},
	}
	picked := pickByStrategy(accs, time.Now())
	if picked == nil {
		t.Fatalf("expected non-nil pick")
	}
	if picked.ID != "more" {
		t.Fatalf("cost-optimized external picked %q, want more (80 remaining)", picked.ID)
	}
}

func TestStrategyCostOptimizedAgentRouterUsesExternalCredits(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "cost-optimized")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	accs := []config.Account{
		{ID: "less", AuthMethod: "agentrouter", ExtCreditLimit: 100, ExtCreditsRemaining: 10},
		{ID: "more", AuthMethod: "agentrouter", ExtCreditLimit: 100, ExtCreditsRemaining: 80},
	}
	if got := pickByStrategy(accs, time.Now()); got == nil || got.ID != "more" {
		t.Fatalf("AgentRouter strategy picked %#v, want more-credit account", got)
	}
}

// TestStrategyResetAwareAvoidsSoonReset verifies reset-aware deprioritises
// accounts whose CodexPrimaryResetAt is within the lead time.
func TestStrategyResetAwareAvoidsSoonReset(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "reset-aware")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	now := time.Now()
	accs := []config.Account{
		{ID: "soon", AuthMethod: "codex", CodexPrimaryUsedPercent: 10, CodexPrimaryResetAt: now.Add(5 * time.Minute).Unix()},
		{ID: "safe", AuthMethod: "codex", CodexPrimaryUsedPercent: 80, CodexPrimaryResetAt: now.Add(2 * time.Hour).Unix()},
	}
	picked := pickByStrategy(accs, now)
	if picked == nil {
		t.Fatalf("expected non-nil pick")
	}
	// "safe" has 80% used but resets in 2h; "soon" has 10% used but resets in 5m.
	// reset-aware should pick "safe" to avoid mid-stream 429.
	if picked.ID != "safe" {
		t.Fatalf("reset-aware picked %q, want safe (resets in 2h)", picked.ID)
	}
}

// TestStrategyResetAwareFallsBackToCostWhenNoResetData verifies that when
// no account has reset data, reset-aware behaves like cost-optimized.
func TestStrategyResetAwareFallsBackToCostWhenNoResetData(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "reset-aware")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	accs := []config.Account{
		{ID: "high", AuthMethod: "codex", CodexPrimaryUsedPercent: 90},
		{ID: "low", AuthMethod: "codex", CodexPrimaryUsedPercent: 10},
	}
	picked := pickByStrategy(accs, time.Now())
	if picked == nil {
		t.Fatalf("expected non-nil pick")
	}
	if picked.ID != "low" {
		t.Fatalf("reset-aware (no reset data) picked %q, want low (10%% used)", picked.ID)
	}
}

// TestStrategyDoesNotApplyForSmallPool verifies the strategy is skipped
// when the pool has fewer than strategyMinPoolSize accounts.
func TestStrategyDoesNotApplyForSmallPool(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "cost-optimized")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	// 5 accounts — below the 20 threshold.
	accs := make([]config.Account, 5)
	for i := range accs {
		accs[i] = config.Account{ID: string(rune('a' + i)), AuthMethod: "codex"}
	}
	p := &AccountPool{accounts: accs}
	if p.strategyShouldApply() {
		t.Fatalf("strategy should not apply for pool of 5 (< 20 threshold)")
	}
}

// TestStrategyAppliesForLargePool verifies the strategy activates when
// the pool has >= strategyMinPoolSize unique accounts.
func TestStrategyAppliesForLargePool(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "cost-optimized")
	defer config.SetStringSetting("poolRoutingStrategy", "")

	// 20 unique accounts — at the threshold.
	accs := make([]config.Account, 20)
	for i := range accs {
		accs[i] = config.Account{ID: string(rune('a' + i)), AuthMethod: "codex"}
	}
	p := &AccountPool{accounts: accs}
	if !p.strategyShouldApply() {
		t.Fatalf("strategy should apply for pool of 20 (>= 20 threshold)")
	}
}

// TestStrategyInvalidValueFallsBackToRoundRobin verifies an unknown
// strategy value falls back to round-robin.
func TestStrategyInvalidValueFallsBackToRoundRobin(t *testing.T) {
	strategyTestSetup(t)
	config.SetStringSetting("poolRoutingStrategy", "nonsense")
	defer config.SetStringSetting("poolRoutingStrategy", "")
	if got := poolRoutingStrategy(); got != "round-robin" {
		t.Fatalf("invalid strategy = %q, want round-robin fallback", got)
	}
}
