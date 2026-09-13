package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"sync"
	"testing"
)

func enableAdaptive(t *testing.T, policy config.AdaptiveRoutingConfig) {
	t.Helper()
	if err := config.UpdateAdaptiveRouting(policy); err != nil {
		t.Fatalf("UpdateAdaptiveRouting: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateAdaptiveRouting(config.AdaptiveRoutingConfig{}) })
}

// newAdaptiveHandler wires a fresh config plus one pool entry per catalog key.
// Account IDs must be unique per test because the pool is a process singleton
// and keeps cooldown/model-lock state keyed by ID.
func newAdaptiveHandler(t *testing.T, catalog map[string][]string) (*Handler, *accountpool.AccountPool) {
	t.Helper()
	mustInitConfig(t)
	for id := range catalog {
		if err := config.AddAccount(config.Account{
			ID:          id,
			Email:       id,
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/" + id,
		}); err != nil {
			t.Fatalf("AddAccount(%s): %v", id, err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()
	for id, models := range catalog {
		p.SetModelList(id, models)
	}
	return &Handler{
		pool:         p,
		promptCache:  newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: newTestTracker(),
	}, p
}

func TestClassifyAdaptiveSignalCoversIntentAndComplexity(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sig            adaptiveSignal
		category, tier string
	}{
		{"english greeting", adaptiveSignal{Text: "hello", CurrentChars: 5}, "general", "fast"},
		{"vietnamese greeting", adaptiveSignal{Text: "chào", CurrentChars: 5}, "general", "fast"},
		{"vietnamese coding", adaptiveSignal{Text: "giúp em sửa lỗi trong mã nguồn", CurrentChars: 30}, "coding", "balanced"},
		{"english research", adaptiveSignal{Text: "compare these two papers", CurrentChars: 24}, "research", "balanced"},
		{"vietnamese creative", adaptiveSignal{Text: "viết một bài viết ngắn", CurrentChars: 22}, "creative", "balanced"},
		{"image outranks coding words", adaptiveSignal{Text: "debug this screenshot", HasImage: true, CurrentChars: 21}, "vision", "balanced"},
		{"active tool result", adaptiveSignal{Text: "tiếp tục", HasTools: true, CurrentChars: 8}, "coding", "strong"},
		{"deep multi-signal task", adaptiveSignal{Text: "audit the architecture and then optimize performance", CurrentChars: 51}, "general", "strong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			category, tier, reason := classifyAdaptiveSignal(tc.sig)
			if category != tc.category || tier != tc.tier {
				t.Fatalf("category/tier = %q/%q, want %q/%q (reason=%q)", category, tier, tc.category, tc.tier, reason)
			}
			if reason == "" {
				t.Fatal("classification produced no reason")
			}
		})
	}
}

// A long system prompt or a long history must price the request honestly without
// upgrading a one-word turn onto a strong model.
func TestAdaptiveSignalSeparatesTurnSizeFromRequestSize(t *testing.T) {
	initConfigForTests(t)
	scaffolding := strings.Repeat("system scaffolding sentence. ", 500)

	openaiSig := adaptiveSignalFromOpenAI(&OpenAIRequest{
		Model: "omni-auto",
		Messages: []OpenAIMessage{
			{Role: "system", Content: scaffolding},
			{Role: "user", Content: "hi"},
		},
	})
	if openaiSig.CurrentChars > 8 {
		t.Fatalf("current turn size = %d, want the short user turn only", openaiSig.CurrentChars)
	}
	if openaiSig.InputChars < 1000 {
		t.Fatalf("cost estimate = %d chars, want the whole request counted", openaiSig.InputChars)
	}
	if _, tier, _ := classifyAdaptiveSignal(openaiSig); tier != "fast" {
		t.Fatalf("tier = %q, want fast: scaffolding must not upgrade a greeting", tier)
	}

	claudeSig := adaptiveSignalFromClaude(&ClaudeRequest{
		Model:    "omni-auto",
		System:   scaffolding,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	})
	if claudeSig.CurrentChars > 8 {
		t.Fatalf("claude current turn size = %d, want the short user turn only", claudeSig.CurrentChars)
	}
	// Both protocols must price the same conversation comparably, otherwise a
	// cost objective picks a different model per client.
	if claudeSig.InputChars < 1000 {
		t.Fatalf("claude cost estimate = %d chars, want the whole request counted", claudeSig.InputChars)
	}
}

// Claude Code declares its whole toolset on every turn. Declared tools are
// capability metadata, not evidence that this turn is an agent step.
func TestAdaptiveSignalIgnoresDeclaredToolsWithoutActiveToolTurn(t *testing.T) {
	initConfigForTests(t)
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = "read_file"

	sig := adaptiveSignalFromOpenAI(&OpenAIRequest{
		Model:    "omni-auto",
		Messages: []OpenAIMessage{{Role: "user", Content: "hello"}},
		Tools:    []OpenAITool{tool},
	})
	if sig.HasTools {
		t.Fatal("declared tools alone marked the turn as a tool turn")
	}
	if _, tier, _ := classifyAdaptiveSignal(sig); tier != "fast" {
		t.Fatalf("tier = %q, want fast for a greeting with declared tools", tier)
	}

	active := adaptiveSignalFromOpenAI(&OpenAIRequest{
		Model: "omni-auto",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "run the build"},
			{Role: "tool", ToolCallID: "call_1", Content: "exit code 1"},
		},
	})
	if !active.HasTools {
		t.Fatal("a trailing tool result must mark the turn as a tool turn")
	}
}

// The quality floor decides the primary model. It must not delete the weaker
// candidates an operator explicitly configured as fallbacks, and a temporarily
// unavailable model must lose its lead without leaving the chain.
func TestAdaptiveRankingPrefersCapacityAndKeepsDegradedFallback(t *testing.T) {
	h, p := newAdaptiveHandler(t, map[string][]string{
		"rank-opus": {"claude-opus-5"},
		"rank-luna": {"gpt-5.6-luna"},
	})
	enableAdaptive(t, config.AdaptiveRoutingConfig{
		Enabled:      true,
		Objective:    "cost",
		DefaultRoute: []string{"claude-opus-5", "gpt-5.6-luna"},
	})
	sig := adaptiveSignal{
		Text:         "audit the architecture and then optimize performance for production",
		CurrentChars: 67,
		InputChars:   4000,
	}

	decision := h.resolveAdaptiveModel("omni-auto", sig)
	if !decision.Intercepted || decision.Tier != "strong" {
		t.Fatalf("decision = %+v, want an intercepted strong-tier route", decision)
	}
	if decision.Selected != "claude-opus-5" {
		t.Fatalf("selected = %q, want claude-opus-5: a cost objective must not buy a weak primary for a strong task", decision.Selected)
	}
	if len(decision.Models) != 2 || decision.Models[1] != "gpt-5.6-luna" {
		t.Fatalf("chain = %v, want the below-floor candidate retained as fallback", decision.Models)
	}

	// Three transient failures put a per-model lock on the only opus account.
	for i := 0; i < 3; i++ {
		p.RecordError("rank-opus", false, "claude-opus-5")
	}
	t.Cleanup(func() { p.RecordSuccess("rank-opus", "claude-opus-5") })

	degraded := h.resolveAdaptiveModel("omni-auto", sig)
	if degraded.Selected != "gpt-5.6-luna" {
		t.Fatalf("selected = %q, want the available model while opus is locked", degraded.Selected)
	}
	if len(degraded.Models) != 2 || degraded.Models[1] != "claude-opus-5" {
		t.Fatalf("chain = %v, want the locked-but-supported model retained for recovery", degraded.Models)
	}
	if degraded.Recommended != "gpt-5.6-luna" {
		t.Fatalf("recommended = %q, want the ranked winner", degraded.Recommended)
	}
}

func TestAdaptiveChainFlattensCombosAndDeduplicates(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{
		"combo-opus": {"claude-opus-5"},
		"combo-luna": {"gpt-5.6-luna"},
	})
	if _, err := config.AddCombo(config.ComboEntry{
		Name:   "combo-pair",
		Models: []string{"claude-opus-5", "gpt-5.6-luna"},
	}); err != nil {
		t.Fatalf("AddCombo: %v", err)
	}
	enableAdaptive(t, config.AdaptiveRoutingConfig{
		Enabled:      true,
		Objective:    "quality",
		DefaultRoute: []string{"combo-pair", "claude-opus-5"},
	})

	decision := h.resolveAdaptiveModel("omni-auto", adaptiveSignal{Text: "hello", CurrentChars: 5})
	want := []string{"claude-opus-5", "gpt-5.6-luna"}
	if len(decision.Models) != len(want) {
		t.Fatalf("chain = %v, want combos flattened and de-duplicated to %v", decision.Models, want)
	}
	for i, model := range want {
		if decision.Models[i] != model {
			t.Fatalf("chain[%d] = %q, want %q (full chain %v)", i, decision.Models[i], model, decision.Models)
		}
	}
}

// Shadow mode must serve the existing default route while still reporting what
// the adaptive policy would have chosen.
func TestAdaptiveShadowModeServesDefaultRouteAndReportsRecommendation(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{
		"shadow-opus": {"claude-opus-5"},
		"shadow-luna": {"gpt-5.6-luna"},
	})
	enableAdaptive(t, config.AdaptiveRoutingConfig{
		Enabled:      true,
		ShadowMode:   true,
		Objective:    "quality",
		DefaultRoute: []string{"gpt-5.6-luna"},
		Routes:       map[string][]string{"general.fast": {"claude-opus-5"}},
	})

	decision := h.resolveAdaptiveModel("omni-auto", adaptiveSignal{Text: "hello", CurrentChars: 5})
	if !decision.Shadow {
		t.Fatal("shadow flag missing from the decision")
	}
	if decision.Recommended != "claude-opus-5" {
		t.Fatalf("recommended = %q, want the policy winner", decision.Recommended)
	}
	if decision.Selected != "gpt-5.6-luna" {
		t.Fatalf("selected = %q, want the default route while shadowing", decision.Selected)
	}
}

func TestResolveAdaptiveModelIgnoresExplicitlyNamedModels(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{"explicit-acct": {"claude-opus-5"}})

	if decision := h.resolveAdaptiveModel("omni-auto", adaptiveSignal{Text: "hi"}); decision.Intercepted {
		t.Fatal("a disabled policy intercepted a request")
	}
	enableAdaptive(t, config.AdaptiveRoutingConfig{Enabled: true, DefaultRoute: []string{"claude-opus-5"}})
	if decision := h.resolveAdaptiveModel("claude-opus-5", adaptiveSignal{Text: "hi"}); decision.Intercepted {
		t.Fatal("an explicitly named model was intercepted by adaptive routing")
	}
	decision := h.resolveAdaptiveModel("omni-auto", adaptiveSignal{Text: "hi"})
	if !decision.Intercepted || decision.VirtualModel != "omni-auto" {
		t.Fatalf("decision = %+v, want interception with the virtual model recorded", decision)
	}
}

// Setup-only frames and empty deltas keep the stream uncommitted so a failure
// can still switch models. Real output or a clean terminal event commits.
func TestStreamPreludeCommitsOnlyOnUsefulOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"claude message_start only", `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m","model":"x","content":[]}}` + "\n\n", false},
		{"claude empty text block", `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, false},
		{"claude error before output", `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"boom"}}`, false},
		{"openai empty delta", `data: {"choices":[{"delta":{"content":""}}]}`, false},
		{"claude text delta", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, true},
		{"openai content delta", `data: {"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"responses text delta", `data: {"type":"response.output_text.delta","delta":"hi"}`, true},
		{"tool call", `data: {"choices":[{"delta":{"tool_calls":[{"id":"c1"}]}}]}`, true},
		{"clean terminator", "data: [DONE]\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamPreludeShouldCommit([]byte(tc.data)); got != tc.want {
				t.Fatalf("streamPreludeShouldCommit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStreamingPreludeWriterHoldsSetupThenStreamsDirectly(t *testing.T) {
	dst := httptest.NewRecorder()
	w := newStreamingPreludeWriter(dst)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	setup := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"content\":[]}}\n\n"
	if _, err := w.Write([]byte(setup)); err != nil {
		t.Fatalf("write setup: %v", err)
	}
	if w.committed() {
		t.Fatal("setup-only prelude committed; a failure here could no longer fall back")
	}
	if dst.Body.Len() != 0 {
		t.Fatalf("prelude leaked to the client: %q", dst.Body.String())
	}

	delta := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	if _, err := w.Write([]byte(delta)); err != nil {
		t.Fatalf("write delta: %v", err)
	}
	if !w.committed() {
		t.Fatal("useful output did not commit the stream")
	}
	if got := dst.Body.String(); !strings.Contains(got, "message_start") || !strings.Contains(got, "hi") {
		t.Fatalf("committed body lost the prelude or the delta: %q", got)
	}
	if got := dst.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("committed Content-Type = %q", got)
	}

	if _, err := w.Write([]byte("data: tail\n\n")); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	if !strings.Contains(dst.Body.String(), "tail") {
		t.Fatal("post-commit writes were buffered instead of streamed")
	}
}

func TestAdaptiveCatalogVisibilityFollowsFeatureFlag(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{"catalog-acct": {"claude-opus-5"}})

	listIDs := func() []string {
		rec := httptest.NewRecorder()
		h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/v1/models status = %d: %s", rec.Code, rec.Body.String())
		}
		return modelIDsFromList(t, rec.Body.Bytes())
	}
	byID := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleModelByID(rec, httptest.NewRequest(http.MethodGet, "/v1/models/omni-auto", nil), "omni-auto")
		return rec
	}

	for _, id := range listIDs() {
		if id == "omni-auto" {
			t.Fatal("disabled adaptive router was advertised in /v1/models")
		}
	}
	if rec := byID(); rec.Code != http.StatusNotFound {
		t.Fatalf("/v1/models/omni-auto status = %d, want 404 while disabled", rec.Code)
	}

	enableAdaptive(t, config.AdaptiveRoutingConfig{Enabled: true, DefaultRoute: []string{"claude-opus-5"}})

	found := false
	for _, id := range listIDs() {
		if id == "omni-auto" {
			found = true
		}
	}
	if !found {
		t.Fatal("enabled adaptive router missing from /v1/models")
	}
	rec := byID()
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models/omni-auto status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var entry map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode by-id entry: %v", err)
	}
	if entry["id"] != "omni-auto" || entry["adaptive"] != true {
		t.Fatalf("by-id entry = %#v, want the adaptive router", entry)
	}
}

// Every protocol must fail the same way when the policy has no usable candidate,
// and must expose the routing trace without leaking prompt text.
func TestAdaptiveUnavailableFailsIdenticallyOnEveryProtocol(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{"unavail-acct": {"claude-opus-5"}})
	enableAdaptive(t, config.AdaptiveRoutingConfig{
		Enabled:      true,
		DefaultRoute: []string{"model-nobody-serves"},
	})

	for _, tc := range []struct {
		name, path, body string
		invoke           func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{
			"claude messages", "/v1/messages",
			`{"model":"omni-auto","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.handleClaudeMessages(w, r) },
		},
		{
			"chat completions", "/v1/chat/completions",
			`{"model":"omni-auto","messages":[{"role":"user","content":"hi"}]}`,
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.handleOpenAIChat(w, r) },
		},
		{
			"responses", "/v1/responses",
			`{"model":"omni-auto","input":"hi","store":false}`,
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.handleOpenAIResponses(w, r) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.invoke(h, rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Omni-Adaptive"); got != "1" {
				t.Fatalf("X-Omni-Adaptive = %q, want 1", got)
			}
			if got := rec.Header().Get("X-Omni-Route-Tier"); got == "" {
				t.Fatal("routing trace tier header missing")
			}
			if body := rec.Body.String(); strings.Contains(body, "hi\"") {
				t.Fatalf("error body echoed prompt text: %s", body)
			}
		})
	}
}

// End-to-end: the ranked chain must actually fall back to the next model when
// the first one is rejected upstream, and the trace must name the first choice.
func TestAdaptiveChainFallsBackToNextModelOnUpstreamRejection(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{
		"fallback-acct": {"alpha-fallback-1", "beta-fallback-2"},
	})
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("UpdatePreferredEndpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("UpdateEndpointFallback: %v", err)
	}
	enableAdaptive(t, config.AdaptiveRoutingConfig{
		Enabled:      true,
		Objective:    "balanced",
		DefaultRoute: []string{"alpha-fallback-1", "beta-fallback-2"},
	})

	var mu sync.Mutex
	var attempts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mu.Lock()
		if strings.Contains(string(body), "alpha-fallback-1") {
			attempts = append(attempts, "alpha-fallback-1")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limit exceeded for this model"}`))
			return
		}
		attempts = append(attempts, "beta-fallback-2")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second candidate answered",
		}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"omni-auto","messages":[{"role":"user","content":"hello"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after fallback: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Omni-Route-Selected"); got != "alpha-fallback-1" {
		t.Fatalf("X-Omni-Route-Selected = %q, want the first ranked candidate", got)
	}
	if !strings.Contains(rec.Body.String(), "second candidate answered") {
		t.Fatalf("client did not receive the fallback model's answer: %s", rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) < 2 || attempts[0] != "alpha-fallback-1" || attempts[len(attempts)-1] != "beta-fallback-2" {
		t.Fatalf("upstream attempts = %v, want the ranked order then the fallback", attempts)
	}
}

func TestAdminAdaptiveRoutingAPIRoundTripAndValidation(t *testing.T) {
	h, _ := newAdaptiveHandler(t, map[string][]string{"admin-acct": {"claude-opus-5"}})

	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.apiUpdateAdaptiveRouting(rec, httptest.NewRequest(http.MethodPut, "/admin/api/adaptive-routing", strings.NewReader(body)))
		return rec
	}

	ok := put(`{"enabled":true,"objective":" QUALITY ","virtualModel":" omni-auto ","defaultRoute":[" claude-opus-5 "],"routes":{"coding.strong":["claude-opus-5"]}}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("valid policy rejected: %d %s", ok.Code, ok.Body.String())
	}
	t.Cleanup(func() { _ = config.UpdateAdaptiveRouting(config.AdaptiveRoutingConfig{}) })

	var saved config.AdaptiveRoutingConfig
	if err := json.Unmarshal(ok.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if saved.Objective != "quality" || saved.VirtualModel != "omni-auto" || len(saved.DefaultRoute) != 1 || saved.DefaultRoute[0] != "claude-opus-5" {
		t.Fatalf("policy was not normalized: %#v", saved)
	}

	getRec := httptest.NewRecorder()
	h.apiGetAdaptiveRouting(getRec, httptest.NewRequest(http.MethodGet, "/admin/api/adaptive-routing", nil))
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), "claude-opus-5") {
		t.Fatalf("GET returned %d %s", getRec.Code, getRec.Body.String())
	}

	for name, body := range map[string]string{
		"unknown field":        `{"enabled":true,"defaltRoute":["claude-opus-5"]}`,
		"unsupported route":    `{"enabled":true,"routes":{"coding.extreme":["claude-opus-5"]}}`,
		"recursive route":      `{"enabled":true,"virtualModel":"router","defaultRoute":["router"]}`,
		"bad objective":        `{"enabled":true,"objective":"cheapest","defaultRoute":["claude-opus-5"]}`,
		"header injection":     `{"enabled":true,"defaultRoute":["claude-opus-5\r\nX-Evil: 1"]}`,
		"out of range quality": `{"enabled":true,"profiles":{"claude-opus-5":{"quality":500}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := put(body); rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid policy accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}

	// A rejected update must not disturb the stored policy.
	current := config.GetAdaptiveRouting()
	if current.Objective != "quality" || len(current.DefaultRoute) != 1 {
		t.Fatalf("stored policy changed after rejected updates: %#v", current)
	}
}
