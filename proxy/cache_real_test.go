package proxy

import (
	"omniproxy/config"
	"testing"
)

// TestUpdateTokensAndCacheFromEventExtractsCacheRead verifies that when the
// upstream event carries a real prompt-cache hit (Kiro's cacheReadInputTokens
// or OpenAI's cached_tokens), the new helper returns it as the third return
// value so the handler can forward it to the client via OnCacheRead.
func TestUpdateTokensAndCacheFromEventExtractsCacheRead(t *testing.T) {
	cases := []struct {
		name          string
		event         map[string]interface{}
		wantCacheRead int
	}{
		{
			name: "kiro cacheReadInputTokens",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"uncachedInputTokens":  float64(100),
					"cacheReadInputTokens": float64(800),
					"cacheWriteInputTokens": float64(50),
				},
			},
			wantCacheRead: 800,
		},
		{
			name: "snake_case cache_read_input_tokens",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"cache_read_input_tokens": float64(1234),
				},
			},
			wantCacheRead: 1234,
		},
		{
			name: "openai cached_tokens",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"cached_tokens": float64(567),
				},
			},
			wantCacheRead: 567,
		},
		{
			name: "no cache fields -> zero",
			event: map[string]interface{}{
				"usage": map[string]interface{}{
					"input_tokens":  float64(900),
					"output_tokens": float64(42),
				},
			},
			wantCacheRead: 0,
		},
		{
			name:          "no usage shape -> zero",
			event:         map[string]interface{}{"content": "hello"},
			wantCacheRead: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotCacheRead := updateTokensAndCacheFromEvent(tc.event, 0, 0)
			if gotCacheRead != tc.wantCacheRead {
				t.Fatalf("cacheRead: got %d, want %d", gotCacheRead, tc.wantCacheRead)
			}
		})
	}
}

// TestBuildOpenAIUsageMapWithCachedTokens verifies the OpenAI usage map
// surfaces the upstream's real cached_tokens via prompt_tokens_details when
// present, and omits the field entirely when there is no cache hit.
func TestBuildOpenAIUsageMapWithCachedTokens(t *testing.T) {
	// With cached tokens -> prompt_tokens_details present.
	u := buildOpenAIUsageMap(1000, 50, 800)
	if u["prompt_tokens"].(int) != 1000 {
		t.Fatalf("prompt_tokens: got %v, want 1000", u["prompt_tokens"])
	}
	if u["completion_tokens"].(int) != 50 {
		t.Fatalf("completion_tokens: got %v, want 50", u["completion_tokens"])
	}
	ptd, ok := u["prompt_tokens_details"].(map[string]int)
	if !ok {
		t.Fatalf("prompt_tokens_details missing or wrong type: %T", u["prompt_tokens_details"])
	}
	if ptd["cached_tokens"] != 800 {
		t.Fatalf("cached_tokens: got %d, want 800", ptd["cached_tokens"])
	}

	// Without cached tokens -> no prompt_tokens_details key.
	u2 := buildOpenAIUsageMap(100, 10, 0)
	if _, ok := u2["prompt_tokens_details"]; ok {
		t.Fatal("prompt_tokens_details should be absent when cachedTokens=0")
	}
}

// TestApplyExternalCacheControlBreakpoints verifies cache_control breakpoints
// are attached at the expected stable prefix boundaries (system, history tail,
// current user) and capped at 4 total, only when the flag is on. The function
// itself is always-on; the handler gates it behind the config flag, so we test
// the function's placement logic directly.
func TestApplyExternalCacheControlBreakpoints(t *testing.T) {
	t.Run("system+history+user with 3+ messages", func(t *testing.T) {
		msgs := []map[string]interface{}{
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": "reply"},
			{"role": "user", "content": "second"},
		}
		applyExternalCacheControl(msgs)
		// system (idx 0), history tail (idx 2 = second-to-last), current user (idx 3)
		if _, ok := msgs[0]["cache_control"]; !ok {
			t.Fatal("system message should get cache_control")
		}
		if _, ok := msgs[2]["cache_control"]; !ok {
			t.Fatal("history tail (second-to-last) should get cache_control")
		}
		if _, ok := msgs[3]["cache_control"]; !ok {
			t.Fatal("current user message should get cache_control")
		}
		// first user (idx 1) is interior history, not a breakpoint.
		if _, ok := msgs[1]["cache_control"]; ok {
			t.Fatal("interior history message should NOT get cache_control")
		}
	})

	t.Run("single user message -> no breakpoints", func(t *testing.T) {
		msgs := []map[string]interface{}{
			{"role": "user", "content": "hi"},
		}
		applyExternalCacheControl(msgs)
		if _, ok := msgs[0]["cache_control"]; ok {
			t.Fatal("single message should not get cache_control (no prefix to cache)")
		}
	})

	t.Run("system only + user -> only system breakpoint", func(t *testing.T) {
		msgs := []map[string]interface{}{
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hi"},
		}
		applyExternalCacheControl(msgs)
		if _, ok := msgs[0]["cache_control"]; !ok {
			t.Fatal("system message should get cache_control")
		}
		// len(msgs) < 3 so no history-tail or current-user breakpoint.
		if _, ok := msgs[1]["cache_control"]; ok {
			t.Fatal("current user with <3 messages should not get cache_control")
		}
	})
}

// TestGetCacheControlPassthroughDefault verifies the config flag defaults to
// false (off) so cache_control is NOT forwarded to upstream providers unless
// the operator explicitly enables it.
func TestGetCacheControlPassthroughDefault(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if config.GetCacheControlPassthrough() {
		t.Fatal("CacheControlPassthrough should default to false")
	}
}
