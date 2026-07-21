package proxy

import (
	"strings"
	"testing"
)

// TestCodexSSEForwardsCachedTokens verifies that processCodexSSELine fires
// OnCacheRead when the Codex /v1/responses stream emits a response.completed
// event whose usage object carries input_tokens_details.cached_tokens. This
// is the path that makes real Codex prompt-cache hits visible to the client.
func TestCodexSSEForwardsCachedTokens(t *testing.T) {
	cases := []struct {
		name          string
		data          string
		wantCacheRead int
		wantFired     bool
	}{
		{
			name: "response.completed with cached_tokens",
			data: `{"type":"response.completed","response":{"usage":{"input_tokens":1500,"output_tokens":42,"input_tokens_details":{"cached_tokens":1200}}}}`,
			wantCacheRead: 1200,
			wantFired:     true,
		},
		{
			name: "response.completed without cached_tokens (no details)",
			data: `{"type":"response.completed","response":{"usage":{"input_tokens":1500,"output_tokens":42}}}`,
			wantCacheRead: 0,
			wantFired:     false,
		},
		{
			name: "response.completed with zero cached_tokens",
			data: `{"type":"response.completed","response":{"usage":{"input_tokens":1500,"output_tokens":42,"input_tokens_details":{"cached_tokens":0}}}}`,
			wantCacheRead: 0,
			wantFired:     false,
		},
		{
			name: "delta event does not fire OnCacheRead",
			data: `{"type":"response.output_text.delta","delta":"hi"}`,
			wantCacheRead: 0,
			wantFired:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCacheRead int
			fired := false
			cb := &KiroStreamCallback{
				OnCacheRead: func(c int) {
					fired = true
					gotCacheRead = c
				},
				OnText: func(string, bool) {},
			}
			toolAccums := map[string]*codexToolAccum{}
			inTok, outTok := 0, 0
			processCodexSSELine("data: "+tc.data, cb, toolAccums, &inTok, &outTok)

			if fired != tc.wantFired {
				t.Fatalf("OnCacheRead fired: got %v, want %v", fired, tc.wantFired)
			}
			if fired && gotCacheRead != tc.wantCacheRead {
				t.Fatalf("cacheRead: got %d, want %d", gotCacheRead, tc.wantCacheRead)
			}
		})
	}
}

// TestCodexJSONForwardsCachedTokens verifies the non-stream Codex path
// (parseCodexResponsesJSON) also forwards cached_tokens via OnCacheRead.
func TestCodexJSONForwardsCachedTokens(t *testing.T) {
	body := strings.NewReader(`{
		"output": [{"type":"message","content":[{"type":"output_text","text":"hi"}]}],
		"usage": {"input_tokens":1500,"output_tokens":42,"input_tokens_details":{"cached_tokens":999}}
	}`)

	var gotCacheRead int
	fired := false
	cb := &KiroStreamCallback{
		OnCacheRead: func(c int) { fired = true; gotCacheRead = c },
	}
	if err := parseCodexResponsesJSON(body, cb); err != nil {
		t.Fatalf("parseCodexResponsesJSON: %v", err)
	}
	if !fired {
		t.Fatal("OnCacheRead should have fired for cached_tokens=999")
	}
	if gotCacheRead != 999 {
		t.Fatalf("cacheRead: got %d, want 999", gotCacheRead)
	}
}
