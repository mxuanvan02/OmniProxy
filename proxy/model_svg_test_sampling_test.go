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
