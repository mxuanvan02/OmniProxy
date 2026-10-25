package proxy

import (
	"strings"
	"testing"
)

// extractServedModel reads the model a provider claims in its own response.
// A resale relay that silently substitutes a cheaper model is the failure this
// exists to catch: the combo asked for A, the provider answered with B, and
// without this check the substitution is invisible in every log.
func TestExtractServedModelFromOpenAIJSON(t *testing.T) {
	body := []byte(`{"id":"x","model":"gpt-5.6-mini","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	if got := extractServedModel(body); got != "gpt-5.6-mini" {
		t.Fatalf("extractServedModel = %q, want gpt-5.6-mini", got)
	}
}

func TestExtractServedModelFromAnthropicJSON(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-5","content":[{"type":"text","text":"hi"}]}`)
	if got := extractServedModel(body); got != "claude-sonnet-5" {
		t.Fatalf("extractServedModel = %q, want claude-sonnet-5", got)
	}
}

func TestExtractServedModelFromSSEPrelude(t *testing.T) {
	// The first chat-completions chunk carries the model; later usage chunks
	// may not. The prelude is what the combo layer has buffered.
	body := []byte("data: {\"id\":\"c1\",\"model\":\"gpt-5.6-sol\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	if got := extractServedModel(body); got != "gpt-5.6-sol" {
		t.Fatalf("extractServedModel = %q, want gpt-5.6-sol", got)
	}
}

func TestExtractServedModelReturnsEmptyWhenAbsentOrMalformed(t *testing.T) {
	for name, body := range map[string][]byte{
		"no model field":  []byte(`{"id":"x","choices":[]}`),
		"empty model":     []byte(`{"model":"","choices":[]}`),
		"malformed json":  []byte(`{"model":"gpt`),
		"empty body":      {},
		"sse without key": []byte("data: {\"error\":{\"message\":\"boom\"}}\n\n"),
	} {
		if got := extractServedModel(body); got != "" {
			t.Errorf("%s: extractServedModel = %q, want empty", name, got)
		}
	}
}

// The served model legitimately differs from the requested string when the
// provider normalises deployment variants or thinking suffixes. The comparison
// must normalise the same way the router does before calling it a mismatch.
func TestServedModelMismatchNormalisesVariants(t *testing.T) {
	cases := []struct {
		requested, served string
		wantMismatch      bool
	}{
		{"gpt-5.6-sol", "gpt-5.6-sol", false},
		{"GPT-5.6-SOL", "gpt-5.6-sol", false},
		{"claude-opus-5[1m]", "claude-opus-5", false},
		{"gpt-5.6-sol", "gpt-5.6-mini", true},
		{"claude-sonnet-5", "qwen3.8-max", true},
	}
	for _, tc := range cases {
		if got := servedModelMismatch(tc.requested, tc.served); got != tc.wantMismatch {
			t.Errorf("servedModelMismatch(%q, %q) = %v, want %v", tc.requested, tc.served, got, tc.wantMismatch)
		}
	}
	if strings.TrimSpace("") != "" {
		t.Fatal("sanity")
	}
}
