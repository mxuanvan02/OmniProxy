package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// Vision rides the chat wire, so each dialect wraps the image differently and a
// body built for the wrong one is rejected with a 400 that reads on the matrix
// as "no vision" on a perfectly capable account. These are the three shapes the
// live adapters actually emit.

func TestProbeVisionRequestBodyMatchesDialect(t *testing.T) {
	cases := []struct {
		name       string
		dialect    string
		wantFields []string
		wantAbsent []string
	}{
		{"responses uses input", "responses", []string{"input", "max_output_tokens"}, []string{"messages", "max_tokens"}},
		{"chat uses messages", "chat", []string{"messages", "max_tokens"}, []string{"input", "max_output_tokens"}},
		{"anthropic uses messages", "anthropic", []string{"messages", "max_tokens"}, []string{"input"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := probeVisionRequestBody(tc.dialect, "qwen3.8-max")
			if body == nil {
				t.Fatal("probeVisionRequestBody returned nil")
			}
			var decoded map[string]interface{}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if decoded["model"] != "qwen3.8-max" {
				t.Errorf("model = %v, want qwen3.8-max", decoded["model"])
			}
			for _, f := range tc.wantFields {
				if _, ok := decoded[f]; !ok {
					t.Errorf("%s body missing %q: %s", tc.dialect, f, body)
				}
			}
			for _, f := range tc.wantAbsent {
				if _, ok := decoded[f]; ok {
					t.Errorf("%s body should not carry %q: %s", tc.dialect, f, body)
				}
			}
			if !strings.Contains(string(body), "image") {
				t.Errorf("%s vision body carries no image part: %s", tc.dialect, body)
			}
		})
	}
}

// The image wrapper differs per dialect and is the part most likely to drift.
// Pinning each spelling is what keeps the probe honest about the wire it tests.
func TestProbeVisionImagePartShape(t *testing.T) {
	t.Run("chat uses nested image_url", func(t *testing.T) {
		body := probeVisionRequestBody("chat", "qwen3.8-max")
		if !strings.Contains(string(body), `"type":"image_url"`) {
			t.Errorf("chat body missing nested image_url part: %s", body)
		}
		if !strings.Contains(string(body), `"url":"data:image/png;base64,`) {
			t.Errorf("chat body missing data URL: %s", body)
		}
	})
	t.Run("responses uses flat input_image", func(t *testing.T) {
		body := probeVisionRequestBody("responses", "qwen3.8-max")
		if !strings.Contains(string(body), `"type":"input_image"`) {
			t.Errorf("responses body missing input_image part: %s", body)
		}
		if strings.Contains(string(body), `"type":"image_url"`) {
			t.Errorf("responses body should not use the chat image_url part: %s", body)
		}
	})
	t.Run("anthropic uses source/base64", func(t *testing.T) {
		body := probeVisionRequestBody("anthropic", "qwen3.8-max")
		if !strings.Contains(string(body), `"type":"image"`) {
			t.Errorf("anthropic body missing image part: %s", body)
		}
		if !strings.Contains(string(body), `"media_type":"image/png"`) {
			t.Errorf("anthropic body missing media_type: %s", body)
		}
	})
}

// Providers reject degenerate images: a 1x1 PNG fails upstream with "width or
// height must be larger than 10", which would read as "no vision".
func TestProbeVisionImageDecodes(t *testing.T) {
	if len(probeVisionImageBase64) == 0 {
		t.Fatal("probe image is empty")
	}
	if !strings.HasPrefix(probeVisionImageBase64, "iVBORw0KGgo") {
		t.Errorf("probe image is not a PNG (bad magic): %s", probeVisionImageBase64[:20])
	}
}

func TestProbeVisionRequestBodyRespectsResponsesTokenFloor(t *testing.T) {
	body := probeVisionRequestBody("responses", "qwen3.8-max")
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	tokens, ok := decoded["max_output_tokens"].(float64)
	if !ok || tokens < 16 {
		t.Fatalf("max_output_tokens = %v, want at least 16", decoded["max_output_tokens"])
	}
}

// Vision is a probeable capability and must be treated as cheap, otherwise the
// default probe run skips it and the matrix never learns the answer.
func TestVisionIsProbeableAndCheap(t *testing.T) {
	if _, ok := probeUpstreamPath(capabilityVision); !ok {
		t.Error("vision has no probe path; it should ride the chat wire")
	}
	if !probeCapabilityIsCheap(capabilityVision) {
		t.Error("vision is not marked cheap; a 16x16 PNG with 1 output token is")
	}
	body, ok := probeRequestBody(capabilityVision, "qwen3.8-max")
	if !ok || len(body) == 0 {
		t.Errorf("probeRequestBody(vision) = %d bytes, ok=%v; want a body", len(body), ok)
	}
}

// A catalog that does publish input types must yield vision from discovery, so
// providers with real metadata are not reduced to probe-only.
func TestDiscoverCapabilitiesFromInputTypes(t *testing.T) {
	got := discoverCapabilitiesFromModels([]ModelInfo{
		{ModelId: "qwen3.8-max", InputTypes: []string{"TEXT", "IMAGE"}},
	})
	if !containsFold(got, capabilityVision) {
		t.Errorf("want %q discovered from InputTypes, got %v", capabilityVision, got)
	}

	absent := discoverCapabilitiesFromModels([]ModelInfo{
		{ModelId: "qwen3.8-max", InputTypes: []string{"TEXT"}},
	})
	if containsFold(absent, capabilityVision) {
		t.Errorf("text-only model must not claim vision, got %v", absent)
	}
}
