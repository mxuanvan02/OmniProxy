package proxy

import "testing"

func TestDynamicPolicyModelLimits(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  int
		wantOutput int
		wantOk     bool
	}{
		{"qwen4-max", 1_000_000, 128_000, true},
		{"qwen3.8-max", 1_000_000, 128_000, true},
		{"qwen3.7-plus", 1_000_000, 128_000, true},
		{"qwen5-max", 1_000_000, 128_000, true}, // future model
		{"claude-opus-5", 1_000_000, 128_000, true},
		{"gemini-3.7-flash", 1_000_000, 65_536, true},
		{"minimax-m3", 1_000_000, 8_192, true},
		{"kimi-k3", 1_000_000, 8_192, true},
		{"gpt-6-astra", 272_000, 128_000, true},
		{"gpt-5.6-luna", 272_000, 128_000, true},
		{"gpt-5.4-mini", 200_000, 100_000, true},
		{"deepseek-v4.1-flash", 128_000, 8_192, true},
		{"glm-5.3", 128_000, 8_192, true},
		{"grok-4.7", 131_072, 8_192, true},
		{"model-S", 0, 0, false}, // SOTA alias must remain false
	}

	for _, tc := range tests {
		in, out, ok := policyModelLimits(tc.model)
		if ok != tc.wantOk || in != tc.wantInput || out != tc.wantOutput {
			t.Errorf("policyModelLimits(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.model, in, out, ok, tc.wantInput, tc.wantOutput, tc.wantOk)
		}
	}
}

func TestExternalModelInfoTokenLimitsFallback(t *testing.T) {
	// External model with no token limits from upstream (like VSLLM)
	info := ModelInfo{
		ModelId:     "qwen4-max",
		External:    true,
		TokenLimits: nil,
	}
	in, out, ok := modelInfoTokenLimits(info)
	if !ok || in != 1_000_000 || out != 128_000 {
		t.Fatalf("modelInfoTokenLimits(qwen4-max) = (%d, %d, %v), want (1000000, 128000, true)", in, out, ok)
	}
}
