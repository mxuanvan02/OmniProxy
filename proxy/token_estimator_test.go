package proxy

import (
	"strings"
	"testing"
)

func TestEstimateOpenAIContentTokensDoesNotTokenizeImageBase64(t *testing.T) {
	imageData := strings.Repeat("A", 3_300_000)
	content := []interface{}{
		map[string]interface{}{"type": "input_text", "text": "inspect this screenshot"},
		map[string]interface{}{
			"type":      "input_image",
			"image_url": "data:image/png;base64," + imageData,
		},
	}

	got := estimateOpenAIContentTokens(content)
	want := estimatedOpenAIImageTokens + estimateApproxTokens("inspect this screenshot")
	if got != want {
		t.Fatalf("expected bounded multimodal estimate %d, got %d", want, got)
	}
	if got >= 10_000 {
		t.Fatalf("image estimate should not scale with base64 length, got %d", got)
	}
}

func TestEstimateOpenAIContentTokensRecognizesNestedImageURL(t *testing.T) {
	content := map[string]interface{}{
		"type": "image_url",
		"image_url": map[string]interface{}{
			"url": "https://example.com/screenshot.png",
		},
	}

	if got := estimateOpenAIContentTokens(content); got != estimatedOpenAIImageTokens {
		t.Fatalf("expected image estimate %d, got %d", estimatedOpenAIImageTokens, got)
	}
}
