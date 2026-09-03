package proxy

import (
	"math"
	"testing"
)

func TestComputeCostGPT56Sol(t *testing.T) {
	// 1M input, 200K cached, 100K output → uncached=800K
	// cost = 800K*5/1M + 200K*0.5/1M + 100K*30/1M = 4 + 0.1 + 3 = 7.1
	got := ComputeCost("gpt-5.6-sol", 1_000_000, 200_000, 100_000)
	want := 4.0 + 0.1 + 3.0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost gpt-5.6-sol: got $%.4f, want $%.4f", got, want)
	}
}

func TestComputeCostGPT56Terra(t *testing.T) {
	// 1M input, 200K cached, 100K output -> uncached=800K.
	// cost = 800K*2/1M + 200K*0.2/1M + 100K*12/1M = 1.6 + 0.04 + 1.2 = 2.84
	got := ComputeCost("gpt-5.6-terra", 1_000_000, 200_000, 100_000)
	want := 1.6 + 0.04 + 1.2
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost gpt-5.6-terra: got $%.4f, want $%.4f", got, want)
	}
}

func TestComputeCostGPT56Luna(t *testing.T) {
	// 1M input, 200K cached, 100K output -> uncached=800K.
	// cost = 800K*0.2/1M + 200K*0.02/1M + 100K*1.2/1M = 0.284
	got := ComputeCost("gpt-5.6-luna", 1_000_000, 200_000, 100_000)
	want := 0.16 + 0.004 + 0.12
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost gpt-5.6-luna: got $%.4f, want $%.4f", got, want)
	}
}

func TestComputeCostClaudeOpus48(t *testing.T) {
	// 1M input, 800K cached, 50K output → uncached=200K
	// cost = 200K*5/1M + 800K*0.5/1M + 50K*25/1M = 1 + 0.4 + 1.25 = 2.65
	got := ComputeCost("claude-opus-4.8", 1_000_000, 800_000, 50_000)
	want := 1.0 + 0.4 + 1.25
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost claude-opus-4.8: got $%.4f, want $%.4f", got, want)
	}
}

func TestComputeCostUnknownModel(t *testing.T) {
	got := ComputeCost("unknown-model", 1_000_000, 500_000, 100_000)
	if got != 0 {
		t.Fatalf("ComputeCost unknown model: got $%.4f, want 0", got)
	}
}

func TestComputeCostPrefixFallback(t *testing.T) {
	// claude-opus-4.9 isn't in the table but should fall back to opus-4.8 pricing
	got := ComputeCost("claude-opus-4.9", 1_000_000, 0, 100_000)
	want := 5.0 + 2.5 // 1M*5/1M + 100K*25/1M
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost claude-opus-4.9 fallback: got $%.4f, want $%.4f", got, want)
	}
}

func TestComputeCostStripsPrefix(t *testing.T) {
	// "codex/gpt-5.6-sol" should resolve to "gpt-5.6-sol" pricing
	got := ComputeCost("codex/gpt-5.6-sol", 1_000_000, 0, 0)
	want := 5.0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost codex/gpt-5.6-sol: got $%.4f, want $%.4f", got, want)
	}
}

func TestComputeCostClampsCachedToInput(t *testing.T) {
	// cached > input should be clamped, not negative
	got := ComputeCost("gpt-5.6-sol", 100_000, 500_000, 50_000)
	// uncached = 100K - 100K (clamped) = 0
	// cost = 0 + 100K*0.5/1M + 50K*30/1M = 0.05 + 1.5 = 1.55
	want := 0.05 + 1.5
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost clamp: got $%.4f, want $%.4f", got, want)
	}
}

func TestEffectiveTokens(t *testing.T) {
	cases := []struct {
		input, cached, output, want int
	}{
		{1_000_000, 200_000, 100_000, 900_000}, // 800K + 100K
		{1_000_000, 800_000, 50_000, 250_000},  // 200K + 50K
		{100_000, 0, 50_000, 150_000},          // no cache
		{100_000, 500_000, 50_000, 50_000},     // clamped cached=input → 0 + 50K
	}
	for _, c := range cases {
		got := EffectiveTokens(c.input, c.cached, c.output)
		if got != c.want {
			t.Errorf("EffectiveTokens(%d, %d, %d) = %d, want %d", c.input, c.cached, c.output, got, c.want)
		}
	}
}

func TestCustomPricingOverride(t *testing.T) {
	// Override gpt-5.6-sol with custom pricing
	SetCustomPricing("gpt-5.6-sol", ModelPricing{InputPerM: 10, CachedPerM: 1, OutputPerM: 60, Source: "custom"})
	got := ComputeCost("gpt-5.6-sol", 1_000_000, 0, 100_000)
	want := 10.0 + 6.0 // 1M*10/1M + 100K*60/1M
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost custom override: got $%.4f, want $%.4f", got, want)
	}
	// Clear override
	SetCustomPricing("gpt-5.6-sol", ModelPricing{})
	got = ComputeCost("gpt-5.6-sol", 1_000_000, 0, 100_000)
	want = 5.0 + 3.0 // back to built-in
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("ComputeCost after clearing override: got $%.4f, want $%.4f", got, want)
	}
}

// A flat per-call override has no per-token rates, so the "is this a real
// price" check must consider PerCallUSD or storing one would delete it and
// silently fall back to the vendor rate.
func TestCustomPricingFlatPerCallOverride(t *testing.T) {
	SetCustomPricing("gpt-5.6-sol", ModelPricing{PerCallUSD: 0.42, Source: "custom"})
	defer SetCustomPricing("gpt-5.6-sol", ModelPricing{})

	if got := ComputeCost("gpt-5.6-sol", 1_000_000, 0, 100_000); math.Abs(got-0.42) > 1e-9 {
		t.Fatalf("flat per-call override: got $%.4f, want $0.4200", got)
	}
	// Token counts are irrelevant to a per-call charge.
	if got := ComputeCost("gpt-5.6-sol", 5, 0, 1); math.Abs(got-0.42) > 1e-9 {
		t.Fatalf("flat per-call override on a tiny request: got $%.4f, want $0.4200", got)
	}
}
