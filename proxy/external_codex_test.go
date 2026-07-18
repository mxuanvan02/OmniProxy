package proxy

import (
	"strings"
	"omniproxy/config"
	"testing"
)

// TestCodexSubscriptionModels verifies the seeded Codex model list contains
// the canonical subscription-tier model IDs the upstream /v1/responses
// endpoint accepts.
func TestCodexSubscriptionModels(t *testing.T) {
	models := codexSubscriptionModels()
	if len(models) == 0 {
		t.Fatal("codexSubscriptionModels returned empty list")
	}
	want := map[string]bool{
		"gpt-5.6":           true,
		"gpt-5.6-sol":       true,
		"gpt-5.6-terra":     true,
		"gpt-5.6-luna":      true,
		"gpt-5.5":           true,
		"gpt-5.4":           true,
		"gpt-5.4-mini":      true,
		"gpt-5.1":           true,
		"gpt-5":             true,
		"o4":                true,
		"o3":                true,
		"codex-mini-latest": true,
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ModelId] = true
		if m.TokenLimits == nil {
			t.Errorf("model %s missing TokenLimits", m.ModelId)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("codexSubscriptionModels missing %q", id)
		}
	}
}

// TestIsCodexAccount verifies the AuthMethod-based discriminator.
func TestIsCodexAccount(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		{"codex", true},
		{"external_openai", false},
		{"social", false},
		{"api_key", false},
		{"", false},
	}
	for _, c := range cases {
		a := &config.Account{AuthMethod: c.method}
		if got := isCodexAccount(a); got != c.want {
			t.Errorf("isCodexAccount(method=%q) = %v, want %v", c.method, got, c.want)
		}
	}
	if isCodexAccount(nil) {
		t.Error("isCodexAccount(nil) should be false")
	}
}

// TestCodexCoalescerBatching verifies that rapid OnText calls are batched
// into fewer downstream calls. With a 24ms tick interval, 100 rapid
// per-token calls should produce far fewer than 100 downstream calls.
func TestCodexCoalescerBatching(t *testing.T) {
	var calls int
	var collected strings.Builder
	target := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			calls++
			collected.WriteString(text)
		},
	}
	c := newCodexCoalescer(target)

	// Simulate 100 per-token deltas arriving in a tight loop (well under
	// the 24ms tick interval between them).
	tokens := []string{"H", "e", "l", "l", "o", ",", " ", "w", "o", "r", "l", "d", "!"}
	for i := 0; i < 100; i++ {
		c.OnText(tokens[i%len(tokens)], false)
	}
	// Terminal event flushes the buffer.
	c.OnComplete(10, 100)

	// Without coalescing, calls would be 100 (one per token). With
	// coalescing, the count should be dramatically lower — at most a
	// handful of flushes.
	if calls > 10 {
		t.Errorf("coalescer produced %d downstream calls, expected <= 10", calls)
	}
	// All text must be preserved (no token loss).
	want := ""
	for i := 0; i < 100; i++ {
		want += tokens[i%len(tokens)]
	}
	if got := collected.String(); got != want {
		t.Errorf("coalescer lost text: got %d bytes, want %d bytes", len(got), len(want))
	}
}

// TestCodexCoalescerThinkingSeparation verifies that thinking and
// non-thinking text are flushed to the right isThinking flag.
func TestCodexCoalescerThinkingSeparation(t *testing.T) {
	type call struct {
		text       string
		isThinking bool
	}
	var calls []call
	target := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			calls = append(calls, call{text, isThinking})
		},
	}
	c := newCodexCoalescer(target)

	c.OnText("reaso", true)
	c.OnText("ning", true)
	c.OnText("out", false)
	c.OnText("put", false)
	c.OnComplete(0, 0)

	// Expect at most 2 flushes (one thinking, one non-thinking), but the
	// key invariant is that no flush mixes thinking and non-thinking.
	for _, cl := range calls {
		if strings.Contains(cl.text, "reaso") && !cl.isThinking {
			t.Errorf("thinking text leaked into non-thinking flush: %q", cl.text)
		}
		if strings.Contains(cl.text, "out") && cl.isThinking {
			t.Errorf("non-thinking text leaked into thinking flush: %q", cl.text)
		}
	}
}

// TestCodexCoalescerFlushesOnToolUse verifies that a pending text buffer is
// flushed before OnToolUse fires (so the client sees text before the tool
// call, not after).
func TestCodexCoalescerFlushesOnToolUse(t *testing.T) {
	var order []string
	target := &KiroStreamCallback{
		OnText:    func(text string, isThinking bool) { order = append(order, "text:"+text) },
		OnToolUse: func(tu KiroToolUse) { order = append(order, "tool:"+tu.Name) },
	}
	c := newCodexCoalescer(target)
	c.OnText("before-tool", false)
	c.OnToolUse(KiroToolUse{ToolUseID: "1", Name: "search"})

	if len(order) != 2 || order[0] != "text:before-tool" || order[1] != "tool:search" {
		t.Errorf("expected [text, tool] ordering, got %v", order)
	}
}
