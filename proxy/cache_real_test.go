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

// TestCacheControlPassthroughPerAccountOverride verifies the tri-state
// per-account override: nil inherits the global switch, while an explicit
// true/false wins over it. This is what makes a single-account canary possible
// instead of betting every external gateway on one global flag.
func TestCacheControlPassthroughPerAccountOverride(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Global is false by default (asserted by the test above).
	enabled := true
	disabled := false

	t.Run("nil override inherits global false", func(t *testing.T) {
		acc := &config.Account{ID: "a"}
		if cacheControlPassthroughEnabled(acc) {
			t.Fatal("nil override should inherit the global false setting")
		}
	})

	t.Run("explicit true overrides global false", func(t *testing.T) {
		acc := &config.Account{ID: "a", CacheControlPassthrough: &enabled}
		if !cacheControlPassthroughEnabled(acc) {
			t.Fatal("explicit true should enable passthrough for this account")
		}
	})

	t.Run("explicit false stays off", func(t *testing.T) {
		acc := &config.Account{ID: "a", CacheControlPassthrough: &disabled}
		if cacheControlPassthroughEnabled(acc) {
			t.Fatal("explicit false should keep passthrough off")
		}
	})

	t.Run("nil account falls back to global", func(t *testing.T) {
		if cacheControlPassthroughEnabled(nil) {
			t.Fatal("nil account should fall back to the global false setting")
		}
	})
}

// TestExternalRequestBodyHonoursPerAccountPassthrough verifies the override is
// actually wired into the outbound request body, not just readable in
// isolation. Without the wiring, a canary account would silently keep sending
// uncached prompts.
func TestExternalRequestBodyHonoursPerAccountPassthrough(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// Build the payload through the real translator so the shape matches
	// production exactly (including the system-priming pair it injects).
	newPayload := func() *KiroPayload {
		req := &OpenAIRequest{
			Model: "gpt-4o",
			Messages: []OpenAIMessage{
				{Role: "system", Content: "You are a helpful assistant."},
				{Role: "user", Content: "first turn"},
				{Role: "assistant", Content: "first reply"},
				{Role: "user", Content: "second turn"},
			},
		}
		payload := OpenAIToKiro(req, false)
		payload.OriginalModel = "gpt-4o"
		return payload
	}

	countCacheControl := func(t *testing.T, body map[string]interface{}) int {
		t.Helper()
		msgs, ok := body["messages"].([]map[string]interface{})
		if !ok {
			t.Fatalf("messages missing or wrong type: %T", body["messages"])
		}
		n := 0
		for _, m := range msgs {
			if _, has := m["cache_control"]; has {
				n++
			}
		}
		return n
	}

	enabled := true

	t.Run("override off -> no cache_control", func(t *testing.T) {
		acc := &config.Account{ID: "off", AuthMethod: "external_openai"}
		body, err := kiroPayloadToOpenAIRequest(newPayload(), acc)
		if err != nil {
			t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
		}
		if n := countCacheControl(t, body); n != 0 {
			t.Fatalf("expected 0 cache_control breakpoints with override off, got %d", n)
		}
	})

	t.Run("override on -> breakpoints attached", func(t *testing.T) {
		acc := &config.Account{ID: "on", AuthMethod: "external_openai", CacheControlPassthrough: &enabled}
		body, err := kiroPayloadToOpenAIRequest(newPayload(), acc)
		if err != nil {
			t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
		}
		if n := countCacheControl(t, body); n == 0 {
			t.Fatal("expected cache_control breakpoints with per-account override on, got none")
		}
	})
}

// Minimum cacheable prefix length is per-model and does not follow model size or
// recency: Opus 5 caches from 512 tokens while Opus 4.5/4.6 need 4096. The old
// "opus means 2048" rule was wrong in both directions, and because an
// under-length prefix is silently not cached (no error), only the reported
// numbers revealed it.
func TestMinCacheableTokensForModelFollowsDocumentedThresholds(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-5", 512},
		{"claude-fable-5", 512},
		{"claude-mythos-5", 512},
		{"claude-mythos-preview", 2048},
		{"claude-opus-4.8", 1024},
		{"claude-opus-4-8", 1024},
		{"claude-opus-4.7", 2048},
		{"claude-opus-4.6", 4096},
		{"claude-opus-4.5", 4096},
		{"claude-opus-4-5", 4096},
		{"claude-haiku-4.5", 4096},
		{"claude-haiku-3.5", 2048},
		// Sonnet and anything unrecognised fall back to the documented 1024.
		{"claude-sonnet-5", 1024},
		{"claude-sonnet-4.6", 1024},
		{"gpt-5.6-sol", 1024},
		{"", 1024},
	}
	for _, tc := range cases {
		if got := minCacheableTokensForModel(tc.model); got != tc.want {
			t.Errorf("minCacheableTokensForModel(%q): got %d, want %d", tc.model, got, tc.want)
		}
	}
}

// primedPayload builds the shape the translators emit when the client sent a
// system message: history[0]=user(system), history[1]=assistant("I will follow").
func primedPayload(systemPrompt string, turns ...string) *KiroPayload {
	p := &KiroPayload{}
	history := []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: systemPrompt}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
	}
	for i, turn := range turns {
		if i%2 == 0 {
			history = append(history, KiroHistoryMessage{
				UserInputMessage: &KiroUserInputMessage{Content: turn},
			})
			continue
		}
		history = append(history, KiroHistoryMessage{
			AssistantResponseMessage: &KiroAssistantResponseMessage{Content: turn},
		})
	}
	p.ConversationState.History = history
	return p
}

// unprimedPayload builds a conversation with no system prompt at all. The first
// user turn deliberately avoids the "You are " prefix so it is not mistaken for
// a leading user-only system prompt. convID mirrors what the translators set
// before truncation runs.
func unprimedPayload(convID string, turns ...string) *KiroPayload {
	p := &KiroPayload{}
	p.ConversationState.ConversationID = convID
	history := make([]KiroHistoryMessage, 0, len(turns))
	for i, turn := range turns {
		if i%2 == 0 {
			history = append(history, KiroHistoryMessage{
				UserInputMessage: &KiroUserInputMessage{Content: turn},
			})
			continue
		}
		history = append(history, KiroHistoryMessage{
			AssistantResponseMessage: &KiroAssistantResponseMessage{Content: turn},
		})
	}
	p.ConversationState.History = history
	return p
}

// A conversation without a system prompt must still get a routing key. Returning
// an empty key dropped sticky affinity for the whole conversation and omitted
// prompt_cache_key upstream — measured as the dominant behaviour on the most
// expensive model in the pool.
func TestPayloadCacheKeyFallsBackToConversationPrefix(t *testing.T) {
	t.Run("system prompt present -> key from instructions", func(t *testing.T) {
		key := payloadCacheKey(primedPayload("You are a helpful assistant.", "hello"))
		if key == "" {
			t.Fatal("primed payload produced no cache key")
		}
		want := codexCacheKey("You are a helpful assistant.")
		if key != want {
			t.Fatalf("key not derived from instructions: got %q, want %q", key, want)
		}
	})

	t.Run("no system prompt -> key from conversation id", func(t *testing.T) {
		key := payloadCacheKey(unprimedPayload("conv-a", "summarise this report", "sure"))
		if key == "" {
			t.Fatal("unprimed payload produced no cache key: affinity would be off")
		}
	})

	t.Run("no history -> empty key", func(t *testing.T) {
		if key := payloadCacheKey(unprimedPayload("conv-a")); key != "" {
			t.Fatalf("empty history should not produce a key, got %q", key)
		}
		if key := payloadCacheKey(nil); key != "" {
			t.Fatalf("nil payload should not produce a key, got %q", key)
		}
	})

	// No conversation id means nothing stable to key on, so rotation is the
	// honest answer rather than inventing an unstable key.
	t.Run("no conversation id -> empty key", func(t *testing.T) {
		if key := payloadCacheKey(unprimedPayload("", "summarise this report")); key != "" {
			t.Fatalf("missing conversation id should not produce a key, got %q", key)
		}
	})

	// The whole point of the fallback: the key must not change as the
	// conversation grows, otherwise every turn repins to a fresh account and
	// the fallback is worse than useless.
	t.Run("key is stable as conversation grows", func(t *testing.T) {
		first := payloadCacheKey(unprimedPayload("conv-a", "summarise this report"))
		grown := payloadCacheKey(unprimedPayload(
			"conv-a", "summarise this report", "sure", "now translate it", "done",
		))
		if first == "" {
			t.Fatal("no key for first turn")
		}
		if first != grown {
			t.Fatalf("key changed as conversation grew: %q -> %q", first, grown)
		}
	})

	// Regression: keying on the first history turn drifted mid-conversation
	// because truncatePayloadToLimit drops the oldest turns once a payload
	// outgrows the size limit, and an unprimed payload has no priming pair
	// protecting the head. Live traffic showed one conversation producing three
	// different keys in two minutes. The id survives truncation, so the key must
	// hold even when the opening turns are gone.
	t.Run("key survives history truncation", func(t *testing.T) {
		full := payloadCacheKey(unprimedPayload(
			"conv-a", "opening turn", "reply", "second turn", "reply two",
		))
		truncated := payloadCacheKey(unprimedPayload(
			"conv-a", "second turn", "reply two",
		))
		if full != truncated {
			t.Fatalf("key drifted after truncation: %q -> %q", full, truncated)
		}
	})

	t.Run("different conversations get different keys", func(t *testing.T) {
		a := payloadCacheKey(unprimedPayload("conv-a", "summarise this report"))
		b := payloadCacheKey(unprimedPayload("conv-b", "summarise this report"))
		if a == b {
			t.Fatalf("distinct conversations collided on key %q", a)
		}
	})

	// The fallback key must never collide with an instructions key of the same
	// text, or two different routing scopes would share one sticky entry.
	t.Run("fallback key is namespaced away from instructions key", func(t *testing.T) {
		id := "conv-a"
		if payloadCacheKey(unprimedPayload(id, "hello")) == codexCacheKey(id) {
			t.Fatal("conversation-id key collides with instructions key")
		}
	})
}
