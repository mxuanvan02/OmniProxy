package proxy

import (
	"strings"
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

// The whole measurement rests on the prompt going upstream bare: no injected
// thinking preamble, and no reasoning level requested of any dialect. If either
// leaked in, every result would be describing the proxy's configuration rather
// than how the model chose to spend its own budget, and the comparison would be
// measuring the test harness.
func TestBuildSVGTestPayloadSendsTheBarePrompt(t *testing.T) {
	const prompt = "draw a pelican riding a bicycle as SVG"
	req := &OpenAIRequest{
		Model:     "qwen3.8-max-cn",
		Messages:  []OpenAIMessage{{Role: "user", Content: prompt}},
		MaxTokens: externalAnthropicDefaultMaxTokens,
	}

	payload := buildSVGTestPayload(req, "qwen3.8-max-cn")

	if payload.InferenceConfig == nil {
		t.Fatal("payload lost its inference config")
	}
	if got := payload.InferenceConfig.ReasoningEffort; got != "" {
		t.Errorf("reasoning effort = %q, want empty so no dialect turns thinking on", got)
	}
	if !payload.InferenceConfig.HasTemperature || payload.InferenceConfig.Temperature != svgTestDeterministicTemperature {
		t.Errorf("temperature = %+v, want the pinned %v", payload.InferenceConfig, svgTestDeterministicTemperature)
	}

	// The user turn must still be the operator's own words.
	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(got, prompt) {
		t.Errorf("payload user content lost the operator's text: %q", got)
	}
	if strings.Contains(got, ThinkingModePrompt) {
		t.Error("payload user content contains ThinkingModePrompt; the test must send the prompt as typed")
	}

	// The translator injects a forced-reasoning preamble as a system priming pair
	// at the head of history (translator.go:1361), and records that it did so.
	// Neither must happen here: with no system message in the request there is
	// nothing to prime with, and thinking must not be switched on for the model.
	if payload.hasPriming {
		t.Error("payload carries system priming; a bare prompt should inject none")
	}
	for i, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, ThinkingModePrompt) {
			t.Errorf("history[%d] contains ThinkingModePrompt; reasoning must not be forced on", i)
		}
	}
}

// A model family that rejects a temperature override must keep its own sampling
// rather than be handed a pin that makes the upstream answer HTTP 400 — a 400
// would be recorded as the model failing to draw, which is not what happened.
func TestBuildSVGTestPayloadLeavesRejectingFamiliesAlone(t *testing.T) {
	req := &OpenAIRequest{
		Model:    "gpt-5.2",
		Messages: []OpenAIMessage{{Role: "user", Content: "draw"}},
	}
	payload := buildSVGTestPayload(req, "gpt-5.2")
	if payload.InferenceConfig == nil {
		t.Fatal("payload lost its inference config")
	}
	if payload.InferenceConfig.HasTemperature {
		t.Errorf("temperature pinned for a family that rejects overrides: %+v", payload.InferenceConfig)
	}
	if payload.InferenceConfig.ReasoningEffort != "" {
		t.Errorf("reasoning effort = %q, want empty", payload.InferenceConfig.ReasoningEffort)
	}
}
