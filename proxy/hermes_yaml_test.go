package proxy

import (
	"strings"
	"testing"
)

// A YAML mapping key that starts with a flow indicator breaks the document:
// the parser reads "[opencode]" as a flow sequence and then fails on the
// remainder, taking down every model below it. Reseller upstreams hand back
// exactly these IDs, so quoting has to cover them.
func TestYAMLQuoteIfNeededQuotesFlowIndicators(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"[opencode]deepseek-v4-flash", `"[opencode]deepseek-v4-flash"`},
		{"[opencode]deepseek-v4-pro", `"[opencode]deepseek-v4-pro"`},
		{"{json}model", `"{json}model"`},
		{"gpt-5.1", "gpt-5.1"}, // plain IDs stay unquoted
		{"claude-sonnet-5", "claude-sonnet-5"},
	}
	for _, tc := range cases {
		if got := yamlQuoteIfNeeded(tc.in); got != tc.want {
			t.Errorf("yamlQuoteIfNeeded(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The provider block is what gets written into ~/.hermes/config.yaml, so every
// model key inside it has to be safe to parse.
func TestHermesProviderBlockQuotesBracketedModelKeys(t *testing.T) {
	catalog := []ModelInfo{
		{ModelId: "[opencode]deepseek-v4-flash"},
		{ModelId: "glm-5.3"},
	}
	block := hermesProviderBlock("http://localhost:20131/v1", "test-key", catalog)

	if strings.Contains(block, "      [opencode]") {
		t.Errorf("provider block emits unquoted bracket key:\n%s", block)
	}
	if !strings.Contains(block, `      "[opencode]deepseek-v4-flash":`) {
		t.Errorf("provider block missing quoted bracket key:\n%s", block)
	}
	// Plain IDs must stay unquoted so the block stays readable.
	if !strings.Contains(block, "      glm-5.3:") {
		t.Errorf("provider block should leave plain IDs unquoted:\n%s", block)
	}
}
