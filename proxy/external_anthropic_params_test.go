package proxy

import (
	"testing"
)

// The Messages API accepts a narrower set of parameter values than the OpenAI
// dialects — temperature in [0,1], max_tokens mandatory, thinking budget below
// the ceiling — and rejects rather than clamps. Each case below is a request
// that would otherwise come back as a 400 the caller cannot recover from.

func TestAnthropicSamplingParamsFollowTheAPIRules(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *InferenceConfig
		maxTokens int
		hasTemp   bool
		temp      float64
		thinking  bool
	}{
		{
			// The API's own default applies when the client named no ceiling.
			name:      "no inference config",
			maxTokens: externalAnthropicDefaultMaxTokens,
		},
		{
			name:      "temperature above the API ceiling is dropped",
			cfg:       &InferenceConfig{MaxTokens: 4096, Temperature: 1.5},
			maxTokens: 4096,
		},
		{
			name:      "temperature at the ceiling is kept",
			cfg:       &InferenceConfig{MaxTokens: 4096, Temperature: 1, HasTemperature: true},
			maxTokens: 4096,
			hasTemp:   true,
			temp:      1,
		},
		{
			// Zero means "unset" unless the client pinned it explicitly: sending
			// an unpinned zero would force greedy decoding on a client that
			// never asked for it.
			name:      "zero temperature is omitted",
			cfg:       &InferenceConfig{MaxTokens: 4096, Temperature: 0},
			maxTokens: 4096,
		},
		{
			name:      "explicit greedy pin is kept",
			cfg:       &InferenceConfig{MaxTokens: 4096, Temperature: 0, HasTemperature: true},
			maxTokens: 4096,
			hasTemp:   true,
			temp:      0,
		},
		{
			name:      "reasoning effort above the budget enables thinking",
			cfg:       &InferenceConfig{MaxTokens: 8192, ReasoningEffort: "high"},
			maxTokens: 8192,
			thinking:  true,
		},
		{
			// The API rejects budget_tokens >= max_tokens, so a ceiling that
			// cannot hold the budget is sent without a thinking block.
			name:      "ceiling at the budget omits thinking",
			cfg:       &InferenceConfig{MaxTokens: externalAnthropicThinkingBudget, ReasoningEffort: "high"},
			maxTokens: externalAnthropicThinkingBudget,
		},
		{
			name:      "no reasoning effort omits thinking",
			cfg:       &InferenceConfig{MaxTokens: 8192},
			maxTokens: 8192,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]interface{}{}
			payload := &KiroPayload{InferenceConfig: tc.cfg}
			applyAnthropicSamplingParams(body, payload)

			if got := body["max_tokens"]; got != tc.maxTokens {
				t.Fatalf("max_tokens = %v, want %d", got, tc.maxTokens)
			}
			_, hasTemp := body["temperature"]
			if hasTemp != tc.hasTemp {
				t.Fatalf("temperature present = %v, want %v (body=%v)", hasTemp, tc.hasTemp, body)
			}
			if tc.hasTemp && body["temperature"] != tc.temp {
				t.Fatalf("temperature = %v, want %v", body["temperature"], tc.temp)
			}
			if _, hasThinking := body["thinking"]; hasThinking != tc.thinking {
				t.Fatalf("thinking present = %v, want %v (body=%v)", hasThinking, tc.thinking, body)
			}
		})
	}
}

// The root "type": "object" is required by the Messages API and absent from the
// schemas clients send for the chat dialect, so it is added rather than assumed.
func TestAnthropicToolSchemaGetsAnObjectRoot(t *testing.T) {
	schema := map[string]interface{}{
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
	}
	got, ok := anthropicToolSchema(schema).(map[string]interface{})
	if !ok {
		t.Fatalf("schema type = %T, want a map", anthropicToolSchema(schema))
	}
	if got["type"] != "object" {
		t.Fatalf("root type = %v, want object", got["type"])
	}
	// A schema that already declares a type must keep it.
	declared, _ := anthropicToolSchema(map[string]interface{}{"type": "array"}).(map[string]interface{})
	if declared["type"] != "array" {
		t.Fatalf("root type = %v, want the declared array", declared["type"])
	}
	// Nothing usable reaches the API as a bare object root rather than a nil.
	empty, _ := anthropicToolSchema(nil).(map[string]interface{})
	if empty["type"] != "object" {
		t.Fatalf("nil schema produced %v", empty)
	}
}

func TestAnthropicToolChoiceVocabulary(t *testing.T) {
	payload := &KiroPayload{ToolNameMap: map[string]string{"read": "Read"}}
	cases := []struct {
		name   string
		choice interface{}
		want   interface{}
	}{
		{"required becomes any", "required", map[string]interface{}{"type": "any"}},
		{"auto", "auto", map[string]interface{}{"type": "auto"}},
		// "none" has no counterpart in the vocabulary; leaving the field out is
		// the API's own auto behaviour.
		{"none is dropped", "none", nil},
		{"unknown string is dropped", "sometimes", nil},
		{"tool choice keeps the restored name", map[string]interface{}{"type": "tool", "name": "read"}, map[string]interface{}{"type": "tool", "name": "Read"}},
		{"openai function shape", map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "read"}}, map[string]interface{}{"type": "tool", "name": "Read"}},
		{"tool choice without a name is dropped", map[string]interface{}{"type": "tool"}, nil},
		{"nil", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicToolChoice(tc.choice, payload)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("choice = %v, want nil", got)
				}
				return
			}
			gotMap, _ := got.(map[string]interface{})
			wantMap, _ := tc.want.(map[string]interface{})
			if len(gotMap) != len(wantMap) {
				t.Fatalf("choice = %v, want %v", got, tc.want)
			}
			for k, v := range wantMap {
				if gotMap[k] != v {
					t.Fatalf("choice[%s] = %v, want %v", k, gotMap[k], v)
				}
			}
		})
	}
}

// Kiro records only the bare subtype, and the API accepts a fixed set of media
// types — an unrecognised one would be a 400 rather than a render.
func TestAnthropicMediaTypeMapping(t *testing.T) {
	cases := map[string]string{
		"png":         "image/png",
		"jpeg":        "image/jpeg",
		"jpg":         "image/jpeg",
		"JPEG":        "image/jpeg",
		"gif":         "image/gif",
		"webp":        "image/webp",
		"":            "image/png",
		"image/tiff":  "image/png",
		"application": "image/png",
	}
	for format, want := range cases {
		if got := anthropicMediaType(format); got != want {
			t.Fatalf("anthropicMediaType(%q) = %q, want %q", format, got, want)
		}
	}
}

func TestAnthropicImageBlockNeedsData(t *testing.T) {
	if block := anthropicImageBlock(KiroImage{Format: "png"}); block != nil {
		t.Fatalf("an image with no bytes produced %v", block)
	}
	img := KiroImage{Format: "jpeg"}
	img.Source.Bytes = "Zm9v"
	block := anthropicImageBlock(img)
	if block == nil {
		t.Fatalf("a complete image produced no block")
	}
	source, _ := block["source"].(map[string]interface{})
	if source["type"] != "base64" || source["media_type"] != "image/jpeg" || source["data"] != "Zm9v" {
		t.Fatalf("source = %v", source)
	}
}

// Manual extended thinking rejects temperature and top_p — the API returns 400
// rather than ignoring them. The builder must drop both when thinking is on so
// a client that sends sampling alongside reasoning does not get an
// unrecoverable failure.
func TestAnthropicThinkingDropsSamplingParams(t *testing.T) {
	body := map[string]interface{}{}
	payload := &KiroPayload{InferenceConfig: &InferenceConfig{
		MaxTokens:       8192,
		Temperature:     0.7,
		TopP:            0.9,
		ReasoningEffort: "high",
	}}
	applyAnthropicSamplingParams(body, payload)

	if _, has := body["thinking"]; !has {
		t.Fatalf("thinking absent; body=%v", body)
	}
	if _, has := body["temperature"]; has {
		t.Fatalf("temperature present with thinking; body=%v", body)
	}
	if _, has := body["top_p"]; has {
		t.Fatalf("top_p present with thinking; body=%v", body)
	}
}

// Manual extended thinking also rejects a tool_choice that forces a call. The
// forced call wins: dropping it would silently break the client's tool loop,
// while dropping thinking only forgoes reasoning depth.
func TestAnthropicForcedToolChoiceDropsThinking(t *testing.T) {
	body := map[string]interface{}{"tool_choice": map[string]interface{}{"type": "any"}}
	payload := &KiroPayload{InferenceConfig: &InferenceConfig{
		MaxTokens:       8192,
		ReasoningEffort: "high",
	}}
	applyAnthropicSamplingParams(body, payload)

	if _, has := body["thinking"]; has {
		t.Fatalf("thinking present with forced tool_choice; body=%v", body)
	}
	choice, _ := body["tool_choice"].(map[string]interface{})
	if choice == nil || choice["type"] != "any" {
		t.Fatalf("tool_choice dropped or changed; body=%v", body)
	}
}

// top_p outside [0,1] is rejected by the Messages API. A client asking for 1.5
// gets the API's default here instead of a 400, matching the existing
// temperature clamping.
func TestAnthropicTopPClampToOne(t *testing.T) {
	body := map[string]interface{}{}
	payload := &KiroPayload{InferenceConfig: &InferenceConfig{MaxTokens: 4096, TopP: 1.5}}
	applyAnthropicSamplingParams(body, payload)

	if _, has := body["top_p"]; has {
		t.Fatalf("top_p above 1 should be dropped; body=%v", body)
	}
}
