package proxy

import "testing"

// TestExternalModelTokenLimitsSchemas covers the field names real
// OpenAI-compatible gateways use for token windows. Each upstream names the
// same concept differently, so discovery must accept all of them rather than
// forcing clients back onto the hard-coded 200K/1M heuristic.
func TestExternalModelTokenLimitsSchemas(t *testing.T) {
	cases := []struct {
		name       string
		limits     interface{}
		tokenLimit interface{}
		ctxWindow  interface{}
		maxInput   interface{}
		maxCtx     interface{}
		maxOutput  interface{}
		wantIn     int
		wantOut    int
		wantNil    bool
	}{
		{
			name:   "xpiki limits max_input_tokens",
			limits: map[string]interface{}{"max_input_tokens": float64(1000000)},
			wantIn: 1000000,
		},
		{
			name:   "limits with input and output",
			limits: map[string]interface{}{"max_input_tokens": float64(272000), "max_output_tokens": float64(128000)},
			wantIn: 272000, wantOut: 128000,
		},
		{
			name:       "camelCase token_limits object",
			tokenLimit: map[string]interface{}{"maxInputTokens": float64(200000), "maxOutputTokens": float64(64000)},
			wantIn:     200000, wantOut: 64000,
		},
		{
			name:      "top-level context_window",
			ctxWindow: float64(1048576),
			wantIn:    1048576,
		},
		{
			name:     "top-level max_input_tokens string",
			maxInput: "400000",
			wantIn:   400000,
		},
		{
			name:   "max_context_tokens alias",
			maxCtx: float64(131072),
			wantIn: 131072,
		},
		{
			name:      "output only still published",
			maxOutput: float64(32000),
			wantOut:   32000,
		},
		{
			name:   "context_length inside limits",
			limits: map[string]interface{}{"context_length": float64(500000)},
			wantIn: 500000,
		},
		{
			name:      "largest wins across sources",
			limits:    map[string]interface{}{"max_input_tokens": float64(200000)},
			ctxWindow: float64(1000000),
			wantIn:    1000000,
		},
		{
			name:    "no metadata yields nil",
			wantNil: true,
		},
		{
			name:    "zero values yield nil",
			limits:  map[string]interface{}{"max_input_tokens": float64(0)},
			wantNil: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := externalModelTokenLimits(c.limits, c.tokenLimit, c.ctxWindow, c.maxInput, c.maxCtx, c.maxOutput)
			if c.wantNil {
				if got != nil {
					t.Fatalf("expected nil token limits, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected token limits, got nil")
			}
			if got.MaxInputTokens != c.wantIn {
				t.Errorf("MaxInputTokens = %d, want %d", got.MaxInputTokens, c.wantIn)
			}
			if got.MaxOutputTokens != c.wantOut {
				t.Errorf("MaxOutputTokens = %d, want %d", got.MaxOutputTokens, c.wantOut)
			}
		})
	}
}

// TestExternalModelTokenLimitsIgnoresPricing guards against reading price
// fields as token counts. Gateways put per-1M prices next to limits, and a
// $5 input price must never become a 5-token window.
func TestExternalModelTokenLimitsIgnoresPricing(t *testing.T) {
	limits := map[string]interface{}{
		"input_price":  float64(5),
		"output_price": float64(25),
		"cached_price": float64(0.5),
	}
	if got := externalModelTokenLimits(limits, nil, nil, nil, nil, nil); got != nil {
		t.Fatalf("pricing fields must not become token limits, got %+v", got)
	}
}

func TestExternalModelPublishedContextUsesHeadroom(t *testing.T) {
	info := ModelInfo{
		ModelId:  "external-model",
		Provider: "external",
		External: true,
		TokenLimits: &ModelTokenLimits{
			MaxInputTokens:  1_000_000,
			MaxOutputTokens: 128_000,
		},
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)
	limits, ok := entry["token_limits"].(map[string]interface{})
	if !ok {
		t.Fatal("external model should publish token_limits")
	}
	if limits["maxInputTokens"] != 800_000 {
		t.Fatalf("published input limit = %#v, want 800000", limits["maxInputTokens"])
	}
	if limits["maxOutputTokens"] != 128_000 {
		t.Fatalf("published output limit = %#v, want 128000", limits["maxOutputTokens"])
	}
}

func TestCodexPublishedContextKeepsUpstreamLimit(t *testing.T) {
	info := ModelInfo{
		ModelId:  "gpt-5.6-luna",
		Provider: "openai-codex",
		TokenLimits: &ModelTokenLimits{
			MaxInputTokens:  272_000,
			MaxOutputTokens: 128_000,
		},
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)
	limits, ok := entry["token_limits"].(map[string]interface{})
	if !ok {
		t.Fatal("Codex model should publish token_limits")
	}
	if limits["maxInputTokens"] != 272_000 {
		t.Fatalf("Codex input limit = %#v, want 272000", limits["maxInputTokens"])
	}
}

func TestExternalWithoutUpstreamLimitDoesNotInheritPolicy(t *testing.T) {
	info := ModelInfo{
		ModelId:  "gpt-5.6-luna",
		Provider: "openai",
		External: true,
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)
	if _, ok := entry["token_limits"]; ok {
		t.Fatalf("external model without upstream metadata inherited token limits: %#v", entry["token_limits"])
	}
}

func TestExternalDefaultOutputTokensByFamily(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-5", 128_000},
		{"claude-sonnet-4.6", 128_000},
		{"claude-fable-5-1", 128_000},
		{"gpt-5.6-luna", 128_000},
		{"o4", 128_000},
		{"codex-mini-latest", 128_000},
		{"deepseek-v4-pro", 32_000},
		{"kimi-k3", 32_000},
		{"qwen3.8-max", 32_000},
		{"gemini-3.1-pro", 32_000},
		{"grok-4.6", 32_000},
		{"muse-spark-1.1", 32_000},
		{"anthropic/claude-opus-5", 128_000},
		{"claude-opus-5-thinking", 128_000},
		{"totally-unknown-model", 16_000},
	}
	for _, c := range cases {
		if got := externalDefaultOutputTokens(c.model); got != c.want {
			t.Errorf("externalDefaultOutputTokens(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestExternalOutputFallbackFillsZeroOnly(t *testing.T) {
	// Upstream publishes input but no output cap -> family default fills output.
	info := ModelInfo{
		ModelId:  "claude-opus-4-8",
		Provider: "gpt2api",
		External: true,
		TokenLimits: &ModelTokenLimits{
			MaxInputTokens: 1_000_000,
		},
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)
	limits, ok := entry["token_limits"].(map[string]interface{})
	if !ok {
		t.Fatal("external model should publish token_limits")
	}
	if limits["maxInputTokens"] != 800_000 {
		t.Fatalf("published input limit = %#v, want 800000", limits["maxInputTokens"])
	}
	if limits["maxOutputTokens"] != 128_000 {
		t.Fatalf("published output limit = %#v, want 128000 (family fallback)", limits["maxOutputTokens"])
	}
}

func TestExternalPublishedOutputAlwaysWins(t *testing.T) {
	// Upstream explicitly published an output cap: fallback must not override it.
	info := ModelInfo{
		ModelId:  "deepseek-v4-pro",
		Provider: "openai",
		External: true,
		TokenLimits: &ModelTokenLimits{
			MaxInputTokens:  800_000,
			MaxOutputTokens: 64_000,
		},
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)
	limits, ok := entry["token_limits"].(map[string]interface{})
	if !ok {
		t.Fatal("external model should publish token_limits")
	}
	if limits["maxOutputTokens"] != 64_000 {
		t.Fatalf("published output limit = %#v, want 64000 (upstream wins)", limits["maxOutputTokens"])
	}
}
