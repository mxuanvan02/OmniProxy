package proxy

import (
	"testing"

	"omniproxy/config"
)

func TestSVGTestTemperatureSkipsFamiliesThatRejectOverrides(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "qwen3.8-max", want: true},
		{model: "claude-opus-5", want: true},
		{model: "glm-4.5-air", want: true},
		{model: "gpt-5.2", want: false},
		{model: "GPT-6-astra", want: false},
		{model: "o3-mini", want: false},
	}
	for _, tt := range tests {
		temp, ok := svgTestTemperature(tt.model)
		if ok != tt.want {
			t.Errorf("svgTestTemperature(%q) ok = %v, want %v", tt.model, ok, tt.want)
		}
		if ok && temp != svgTestDeterministicTemperature {
			t.Errorf("svgTestTemperature(%q) temp = %v, want %v", tt.model, temp, svgTestDeterministicTemperature)
		}
	}
}

func pinnedPayload() *KiroPayload {
	return &KiroPayload{
		OriginalModel:   "qwen3.8-max",
		InferenceConfig: &InferenceConfig{Temperature: 0, HasTemperature: true},
	}
}

func TestChatBuilderForwardsExplicitGreedyPin(t *testing.T) {
	account := &config.Account{Provider: "external_openai", BaseURL: "https://example.test/v1"}

	body, err := kiroPayloadToOpenAIRequest(pinnedPayload(), account)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest() error = %v", err)
	}
	if got, ok := body["temperature"]; !ok || got != float64(0) {
		t.Errorf("chat body temperature = %v (present=%v), want 0", got, ok)
	}

	unpinned := &KiroPayload{InferenceConfig: &InferenceConfig{}}
	body, err = kiroPayloadToOpenAIRequest(unpinned, account)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest() error = %v", err)
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("chat body carries temperature for an unpinned config, want omitted")
	}
}

func TestResponsesBuilderForwardsExplicitGreedyPin(t *testing.T) {
	account := &config.Account{Provider: "external_openai", BaseURL: "https://example.test/v1"}
	opts := responsesDialectOptions{ForwardSamplingParams: true}

	body, err := kiroPayloadToResponsesRequest(pinnedPayload(), account, opts)
	if err != nil {
		t.Fatalf("kiroPayloadToResponsesRequest() error = %v", err)
	}
	if got, ok := body["temperature"]; !ok || got != float64(0) {
		t.Errorf("responses body temperature = %v (present=%v), want 0", got, ok)
	}

	body, err = kiroPayloadToResponsesRequest(&KiroPayload{OriginalModel: "qwen3.8-max", InferenceConfig: &InferenceConfig{}}, account, opts)
	if err != nil {
		t.Fatalf("kiroPayloadToResponsesRequest() error = %v", err)
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("responses body carries temperature for an unpinned config, want omitted")
	}
}

func TestAnthropicBuilderForwardsExplicitGreedyPin(t *testing.T) {
	body := map[string]interface{}{}
	applyAnthropicSamplingParams(body, pinnedPayload())
	if got, ok := body["temperature"]; !ok || got != float64(0) {
		t.Errorf("anthropic body temperature = %v (present=%v), want 0", got, ok)
	}

	body = map[string]interface{}{}
	applyAnthropicSamplingParams(body, &KiroPayload{InferenceConfig: &InferenceConfig{}})
	if _, ok := body["temperature"]; ok {
		t.Errorf("anthropic body carries temperature for an unpinned config, want omitted")
	}

	body = map[string]interface{}{}
	applyAnthropicSamplingParams(body, &KiroPayload{InferenceConfig: &InferenceConfig{Temperature: 1.5, HasTemperature: true}})
	if _, ok := body["temperature"]; ok {
		t.Errorf("anthropic body carries out-of-range temperature, want omitted")
	}
}

func TestOpenAIToKiroMarksExplicitTemperature(t *testing.T) {
	payload := OpenAIToKiro(&OpenAIRequest{Model: "m", Temperature: 0.7}, false)
	if payload.InferenceConfig == nil || !payload.InferenceConfig.HasTemperature || payload.InferenceConfig.Temperature != 0.7 {
		t.Errorf("OpenAIToKiro(temperature=0.7) config = %+v, want HasTemperature with 0.7", payload.InferenceConfig)
	}

	payload = OpenAIToKiro(&OpenAIRequest{Model: "m", MaxTokens: 10}, false)
	if payload.InferenceConfig == nil || payload.InferenceConfig.HasTemperature {
		t.Errorf("OpenAIToKiro(no temperature) config = %+v, want no HasTemperature", payload.InferenceConfig)
	}
}

func TestNormalizeSVGTestEffort(t *testing.T) {
	cases := map[string]string{
		"low": "low", "LOW": "low", "  low ": "low",
		"medium": "medium", "Medium": "medium",
		"high": "high", "max": "max",
		"": "", "extreme": "", "lowx": "", "raw": "", "think": "",
	}
	for in, want := range cases {
		if got := normalizeSVGTestEffort(in); got != want {
			t.Errorf("normalizeSVGTestEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// The sweep walks this ladder low to high, so the levels must be exactly the
// ascending set the UI offers and must not include "max" — the gateways that
// accept it disagree on whether it means anything above "high", which would put
// a guaranteed-empty rung on the curve.
func TestSVGTestEffortLevels(t *testing.T) {
	want := []string{"low", "medium", "high"}
	if len(svgTestEffortLevels) != len(want) {
		t.Fatalf("svgTestEffortLevels = %v, want %v", svgTestEffortLevels, want)
	}
	for i, level := range want {
		if svgTestEffortLevels[i] != level {
			t.Errorf("svgTestEffortLevels[%d] = %q, want %q", i, svgTestEffortLevels[i], level)
		}
		if svgTestEffortRank(level) == 0 {
			t.Errorf("effort level %q ranks as unspecified", level)
		}
	}
	for i := 1; i < len(svgTestEffortLevels); i++ {
		if svgTestEffortRank(svgTestEffortLevels[i-1]) >= svgTestEffortRank(svgTestEffortLevels[i]) {
			t.Errorf("ladder is not ascending at %q then %q", svgTestEffortLevels[i-1], svgTestEffortLevels[i])
		}
	}
}

// Reasoning effort is the only lever the sweep moves, so it must come from one
// place: raw always turns reasoning off no matter what effort is passed, think
// honours an explicit rung and otherwise falls back to the default. If a sweep
// rung and a plain think run could drift apart the curve would not be measuring
// the same thing at each point.
func TestSVGTestReasoningEffort(t *testing.T) {
	tests := []struct {
		mode   string
		effort string
		want   string
	}{
		{"raw", "", ""},
		{"raw", "high", ""},
		{"raw", "max", ""},
		{"think", "", svgTestThinkEffort},
		{"think", "  HIGH ", "high"},
		{"think", "low", "low"},
		{"think", "medium", "medium"},
		{"think", "max", "max"},
		{"think", "extreme", svgTestThinkEffort},
	}
	for _, tt := range tests {
		if got := svgTestReasoningEffort(tt.mode, tt.effort); got != tt.want {
			t.Errorf("svgTestReasoningEffort(%q, %q) = %q, want %q", tt.mode, tt.effort, got, tt.want)
		}
	}
}

// buildSVGTestPayload must put the resolved effort on the payload, pin the
// temperature, and force reasoning off in raw mode. This is what makes a sweep
// rung an actual difference upstream rather than a label stored beside an
// identical request.
func TestBuildSVGTestPayloadAppliesEffort(t *testing.T) {
	req := &OpenAIRequest{
		Model:     "qwen3.8-max-cn",
		Messages:  []OpenAIMessage{{Role: "user", Content: "draw"}},
		MaxTokens: externalAnthropicDefaultMaxTokens,
	}

	think := buildSVGTestPayload(req, "qwen3.8-max-cn", "think", "low")
	if think.InferenceConfig == nil || think.InferenceConfig.ReasoningEffort != "low" {
		t.Errorf("think/low reasoning effort = %+v, want low", think.InferenceConfig)
	}
	if !think.InferenceConfig.HasTemperature || think.InferenceConfig.Temperature != svgTestDeterministicTemperature {
		t.Errorf("think payload temperature = %+v, want the pinned %v", think.InferenceConfig, svgTestDeterministicTemperature)
	}

	raw := buildSVGTestPayload(req, "qwen3.8-max-cn", "raw", "high")
	if raw.InferenceConfig == nil || raw.InferenceConfig.ReasoningEffort != "" {
		t.Errorf("raw payload carried reasoning effort %+v, want it forced off", raw.InferenceConfig)
	}
	if !raw.InferenceConfig.HasTemperature {
		t.Error("raw payload lost the temperature pin")
	}

	// Both modes must share one sampling so the only difference between a raw and
	// a think run is reasoning.
	if think.InferenceConfig.Temperature != raw.InferenceConfig.Temperature {
		t.Errorf("modes disagree on temperature: think=%v raw=%v", think.InferenceConfig.Temperature, raw.InferenceConfig.Temperature)
	}
}
