package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"reflect"
	"strings"
	"testing"
	"time"
)

type claudeSSEEvent struct {
	name string
	data map[string]interface{}
}

func parseClaudeSSEEvents(t *testing.T, body string) []claudeSSEEvent {
	t.Helper()
	var events []claudeSSEEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var event claudeSSEEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.data); err != nil {
					t.Fatalf("decode SSE data: %v", err)
				}
			}
		}
		if event.name != "" {
			events = append(events, event)
		}
	}
	return events
}

func TestThinkingSourceReasoningFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be accepted first")
	}
	if source != thinkingSourceReasoningEvent {
		t.Fatalf("expected source to be reasoning, got %v", source)
	}
	if allowTagSource(&source) {
		t.Fatalf("expected tag source to be rejected after reasoning source selected")
	}
}

func TestClaudeStreamConvertsLateThinkingToVisibleText(t *testing.T) {
	initConfigForTests(t)
	firstText := strings.Repeat("x", 60) + " trên mạn"
	secondText := "g. Không commit hoặc push thay đổi."
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":` + string(mustJSON(t, firstText)) + `}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"late reasoning"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":` + string(mustJSON(t, secondText)) + `}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol"}]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	account := config.Account{
		ID:          "late-thinking-external",
		Email:       "late-thinking-external",
		AuthMethod:  externalAuthMethod,
		AccessToken: "test-key",
		BaseURL:     server.URL,
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add external account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	payload := &KiroPayload{OriginalModel: "gpt-5.6-sol"}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "test late reasoning",
		ModelID: "gpt-5.6-sol",
		Origin:  "AI_EDITOR",
	}

	recorder := httptest.NewRecorder()
	h.handleClaudeStream(recorder, payload, "gpt-5.6-sol", true, claudeThinkingResponseOptions{Format: "thinking"}, 1, nil, "")

	events := parseClaudeSSEEvents(t, recorder.Body.String())
	blockTypes := make(map[int]string)
	var visibleText strings.Builder
	textBlockStarts := 0
	seenTextDelta := false
	lateThinkingBlock := false
	messageDeltaCount := 0
	messageStopCount := 0
	for _, event := range events {
		switch event.name {
		case "content_block_start":
			index := int(event.data["index"].(float64))
			block := event.data["content_block"].(map[string]interface{})
			blockType := block["type"].(string)
			blockTypes[index] = blockType
			if blockType == "text" {
				textBlockStarts++
			}
			if blockType == "thinking" && seenTextDelta {
				lateThinkingBlock = true
			}
		case "content_block_delta":
			index := int(event.data["index"].(float64))
			delta := event.data["delta"].(map[string]interface{})
			if blockTypes[index] == "text" && delta["type"] == "text_delta" {
				visibleText.WriteString(delta["text"].(string))
				seenTextDelta = true
			}
		case "message_delta":
			messageDeltaCount++
			delta := event.data["delta"].(map[string]interface{})
			if delta["stop_reason"] != "end_turn" {
				t.Errorf("stop_reason = %v, want end_turn", delta["stop_reason"])
			}
		case "message_stop":
			messageStopCount++
		}
	}

	gotVisible := visibleText.String()
	if count := strings.Count(gotVisible, "late reasoning"); count != 1 {
		t.Errorf("late reasoning count = %d, want 1 in %q", count, gotVisible)
	}
	if got, want := strings.Replace(gotVisible, "late reasoning", "", 1), firstText+secondText; got != want {
		t.Errorf("visible answer after removing late reasoning = %q, want %q", got, want)
	}
	if lateThinkingBlock {
		t.Errorf("late reasoning created a thinking block after visible text started")
	}
	if textBlockStarts != 1 {
		t.Errorf("text block starts = %d, want 1 continuous block", textBlockStarts)
	}
	if messageDeltaCount != 1 || messageStopCount != 1 {
		t.Errorf("terminal events: message_delta=%d message_stop=%d, want 1 each", messageDeltaCount, messageStopCount)
	}
}

func mustJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return data
}

func runClaudeExternalSSE(t *testing.T, accountID, sse string, thinking bool) []claudeSSEEvent {
	t.Helper()
	initConfigForTests(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol"}]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Email:       accountID,
		AuthMethod:  externalAuthMethod,
		AccessToken: "test-key",
		BaseURL:     server.URL,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("add external account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	payload := &KiroPayload{OriginalModel: "gpt-5.6-sol"}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "test stream ordering",
		ModelID: "gpt-5.6-sol",
		Origin:  "AI_EDITOR",
	}

	recorder := httptest.NewRecorder()
	h.handleClaudeStream(recorder, payload, "gpt-5.6-sol", thinking, claudeThinkingResponseOptions{Format: "thinking"}, 1, nil, "")
	return parseClaudeSSEEvents(t, recorder.Body.String())
}

func TestClaudeStreamPreservesThinkingBeforeVisibleText(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking first"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"Visible answer."}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	events := runClaudeExternalSSE(t, "thinking-first-external", sse, true)
	var blockTypes []string
	var thinkingText, visibleText strings.Builder
	var signatureSeen bool
	blockTypeByIndex := make(map[int]string)
	for _, event := range events {
		switch event.name {
		case "content_block_start":
			index := int(event.data["index"].(float64))
			block := event.data["content_block"].(map[string]interface{})
			blockType := block["type"].(string)
			blockTypeByIndex[index] = blockType
			blockTypes = append(blockTypes, blockType)
		case "content_block_delta":
			index := int(event.data["index"].(float64))
			delta := event.data["delta"].(map[string]interface{})
			switch delta["type"] {
			case "thinking_delta":
				if blockTypeByIndex[index] == "thinking" {
					thinkingText.WriteString(delta["thinking"].(string))
				}
			case "signature_delta":
				signature, _ := delta["signature"].(string)
				signatureSeen = signature != ""
			case "text_delta":
				if blockTypeByIndex[index] == "text" {
					visibleText.WriteString(delta["text"].(string))
				}
			}
		}
	}

	if got, want := strings.Join(blockTypes, ","), "thinking,text"; got != want {
		t.Errorf("content block order = %q, want %q", got, want)
	}
	if got := thinkingText.String(); got != "thinking first" {
		t.Errorf("thinking text = %q, want %q", got, "thinking first")
	}
	if !signatureSeen {
		t.Errorf("expected signature delta before closing thinking block")
	}
	if got := visibleText.String(); got != "Visible answer." {
		t.Errorf("visible text = %q, want %q", got, "Visible answer.")
	}
}

func TestClaudeStreamFlushesTextBeforeToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Use this tool."}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"kiro\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	events := runClaudeExternalSSE(t, "text-tool-external", sse, true)
	var blockTypes []string
	var visibleText strings.Builder
	blockTypeByIndex := make(map[int]string)
	stopReason := ""
	for _, event := range events {
		switch event.name {
		case "content_block_start":
			index := int(event.data["index"].(float64))
			block := event.data["content_block"].(map[string]interface{})
			blockType := block["type"].(string)
			blockTypeByIndex[index] = blockType
			blockTypes = append(blockTypes, blockType)
		case "content_block_delta":
			index := int(event.data["index"].(float64))
			delta := event.data["delta"].(map[string]interface{})
			if blockTypeByIndex[index] == "text" && delta["type"] == "text_delta" {
				visibleText.WriteString(delta["text"].(string))
			}
		case "message_delta":
			delta := event.data["delta"].(map[string]interface{})
			stopReason = delta["stop_reason"].(string)
		}
	}

	if got, want := strings.Join(blockTypes, ","), "text,tool_use"; got != want {
		t.Errorf("content block order = %q, want %q", got, want)
	}
	if got := visibleText.String(); got != "Use this tool." {
		t.Errorf("visible text = %q, want %q", got, "Use this tool.")
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stopReason)
	}
}

func TestClaudeStreamFailsOverAfterEmptyExternalSSE(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	chatTokens := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
		case "/v1/chat/completions":
			chatTokens = append(chatTokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			w.Header().Set("Content-Type", "text/event-stream")
			if len(chatTokens) == 1 {
				// Reproduce Kiro's live failure: a terminal stream with no
				// assistant delta and zero completion tokens.
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n"))
				return
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Opus OK\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	accounts := []config.Account{
		{
			ID:          "kiro-empty",
			Email:       "kiro-empty",
			AuthMethod:  externalAuthMethod,
			AccessToken: "kiro-key",
			BaseURL:     server.URL,
			Enabled:     true,
		},
		{
			ID:          "opus-working",
			Email:       "opus-working",
			AuthMethod:  externalAuthMethod,
			AccessToken: "opus-key",
			BaseURL:     server.URL,
			Enabled:     true,
		},
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %s: %v", account.ID, err)
		}
	}

	p := accountpool.GetPool()
	p.Reload()
	tracker := &UsageTracker{
		ringCap:    10,
		ring:       make([]RequestRecord, 10),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	h := &Handler{
		pool:         p,
		promptCache:  newPromptCacheTracker(defaultPromptCacheTTL),
		usageTracker: tracker,
	}
	payload := &KiroPayload{OriginalModel: "claude-opus-5"}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "Say OK",
		ModelID: "claude-opus-5",
		Origin:  "AI_EDITOR",
	}

	recorder := httptest.NewRecorder()
	h.handleClaudeStream(recorder, payload, "claude-opus-5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if len(chatTokens) != 2 {
		t.Fatalf("chat attempts = %d (%v), want 2", len(chatTokens), chatTokens)
	}
	if chatTokens[0] == chatTokens[1] {
		t.Fatalf("expected failover to a different account, tokens = %v", chatTokens)
	}
	if chatTokens[0] != "kiro-key" && chatTokens[0] != "opus-key" {
		t.Fatalf("unexpected first account token %q", chatTokens[0])
	}
	if chatTokens[1] != "kiro-key" && chatTokens[1] != "opus-key" {
		t.Fatalf("unexpected second account token %q", chatTokens[1])
	}

	events := parseClaudeSSEEvents(t, recorder.Body.String())
	gotNames := make([]string, 0, len(events))
	var visibleText strings.Builder
	for _, event := range events {
		gotNames = append(gotNames, event.name)
		if event.name != "content_block_delta" {
			continue
		}
		delta, _ := event.data["delta"].(map[string]interface{})
		if delta["type"] == "text_delta" {
			if text, ok := delta["text"].(string); ok {
				visibleText.WriteString(text)
			}
		}
	}
	wantNames := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("Claude event order = %v, want %v", gotNames, wantNames)
	}
	if got := visibleText.String(); got != "Opus OK" {
		t.Fatalf("visible text = %q, want Opus OK", got)
	}
}

func TestClaudeNonStreamRetriesNextAccountAfterPreResponseFailure(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID:          "first",
		Enabled:     true,
		AccessToken: "token-first",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:000000000001:profile/first",
	}); err != nil {
		t.Fatalf("add first account: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "second",
		Enabled:     true,
		AccessToken: "token-second",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:000000000002:profile/second",
	}); err != nil {
		t.Fatalf("add second account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestTokens := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		requestTokens = append(requestTokens, token)
		// Fail the first attempted account (whichever it is) so the handler
		// is forced to add it to `excluded` and retry the other one.
		if len(requestTokens) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "retried successfully",
		}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to succeed, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(requestTokens) != 2 {
		t.Fatalf("expected two account attempts, got %v", requestTokens)
	}
	if requestTokens[0] == requestTokens[1] {
		t.Fatalf("expected first account to be excluded before retry, got %v", requestTokens)
	}
	tokenSet := map[string]bool{requestTokens[0]: true, requestTokens[1]: true}
	if !tokenSet["token-first"] || !tokenSet["token-second"] {
		t.Fatalf("expected both accounts to be tried, got %v", requestTokens)
	}

	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "retried successfully" {
		t.Fatalf("expected retried response content, got %#v", resp.Content)
	}
}

func TestThinkingSourceTagFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected tag source to be accepted first")
	}
	if source != thinkingSourceTagBlock {
		t.Fatalf("expected source to be tag, got %v", source)
	}
	if allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be rejected after tag source selected")
	}
}

func TestThinkingSourceSameSourceRemainsAllowed(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected initial tag source selection to succeed")
	}
	if !allowTagSource(&source) {
		t.Fatalf("expected repeated tag source selection to stay allowed")
	}

	source = thinkingSourceUnknown
	if !allowReasoningSource(&source) {
		t.Fatalf("expected initial reasoning source selection to succeed")
	}
	if !allowReasoningSource(&source) {
		t.Fatalf("expected repeated reasoning source selection to stay allowed")
	}
}

func TestValidateOpenAIRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestValidateOpenAIRequestShapeAllowsToolResultFinalTurn(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg != "" {
		t.Fatalf("expected tool-result final turn to be valid, got %q", msg)
	}
}

func TestValidateClaudeRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateClaudeRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestResolveClaudeThinkingModeHonorsRequestThinking(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		thinking     *ClaudeThinkingConfig
		wantModel    string
		wantThinking bool
	}{
		{
			name:         "adaptive request enables thinking",
			model:        "claude-sonnet-4.6",
			thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
			wantModel:    "claude-sonnet-4.6",
			wantThinking: true,
		},
		{
			name:         "enabled request enables thinking",
			model:        "claude-opus-4.5",
			thinking:     &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			wantModel:    "claude-opus-4.5",
			wantThinking: true,
		},
		{
			name:         "disabled request keeps thinking off",
			model:        "claude-opus-4.7",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-opus-4.7",
			wantThinking: false,
		},
		{
			name:         "suffix remains supported when thinking is disabled",
			model:        "claude-sonnet-4.5-thinking",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-sonnet-4.5",
			wantThinking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := resolveClaudeThinkingMode(tc.model, tc.thinking, "-thinking")
			if gotModel != tc.wantModel {
				t.Fatalf("expected model %q, got %q", tc.wantModel, gotModel)
			}
			if gotThinking != tc.wantThinking {
				t.Fatalf("expected thinking=%v, got %v", tc.wantThinking, gotThinking)
			}
		})
	}
}

func TestCloneClaudeRequestForThinkingInjectsPromptWithoutMutatingOriginal(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.6",
		System: "Follow the user instructions.",
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected cloned system prompt to be structured blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	gotPrompt := extractSystemPrompt(cloned.System)
	expected := ThinkingModePrompt + "\n\nFollow the user instructions."
	if gotPrompt != expected {
		t.Fatalf("expected injected system prompt %q, got %q", expected, gotPrompt)
	}
	if original, ok := req.System.(string); !ok || original != "Follow the user instructions." {
		t.Fatalf("expected original request system prompt to stay unchanged, got %#v", req.System)
	}
}

func TestCloneClaudeRequestForThinkingPreservesStructuredSystemBlocks(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": "cached system",
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
					"ttl":  "5m",
				},
			},
		},
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected structured system blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	first, ok := blocks[0].(map[string]interface{})
	if !ok || first["text"] != ThinkingModePrompt+"\n" {
		t.Fatalf("expected first block to be thinking prompt, got %#v", blocks[0])
	}
	second, ok := blocks[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected original system block to remain a map, got %T", blocks[1])
	}
	cacheControl, ok := second["cache_control"].(map[string]interface{})
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("expected original cache_control to be preserved, got %#v", second["cache_control"])
	}
}

func TestThinkingPromptAffectsClaudeTokenEstimate(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	baseTokens := estimateClaudeRequestInputTokens(req)
	thinkingTokens := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(req, true))

	if thinkingTokens <= baseTokens {
		t.Fatalf("expected thinking tokens (%d) to exceed base tokens (%d)", thinkingTokens, baseTokens)
	}
}

func TestValidateClaudeThinkingConfig(t *testing.T) {
	tests := []struct {
		name        string
		thinking    *ClaudeThinkingConfig
		maxTokens   int
		expectError bool
	}{
		{
			name:        "adaptive is valid",
			thinking:    &ClaudeThinkingConfig{Type: "adaptive"},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled requires budget",
			thinking:    &ClaudeThinkingConfig{Type: "enabled"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled requires at least 1024 budget tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 512},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled rejects max tokens zero",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			maxTokens:   0,
			expectError: true,
		},
		{
			name:        "enabled budget must stay below max tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "disabled rejects display",
			thinking:    &ClaudeThinkingConfig{Type: "disabled", Display: "summarized"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "missing type is rejected",
			thinking:    &ClaudeThinkingConfig{},
			maxTokens:   4096,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errMsg := validateClaudeThinkingConfig(tc.thinking, tc.maxTokens)
			if tc.expectError && errMsg == "" {
				t.Fatalf("expected validation error")
			}
			if !tc.expectError && errMsg != "" {
				t.Fatalf("expected thinking config to be valid, got %q", errMsg)
			}
		})
	}
}

func TestResolveClaudeThinkingResponseOptions(t *testing.T) {
	tests := []struct {
		name       string
		thinking   *ClaudeThinkingConfig
		defaultFmt string
		wantFmt    string
		wantOmit   bool
	}{
		{
			name:       "default config is preserved when display unset",
			thinking:   &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			defaultFmt: "think",
			wantFmt:    "think",
			wantOmit:   false,
		},
		{
			name:       "summarized forces official thinking blocks",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "summarized"},
			defaultFmt: "reasoning_content",
			wantFmt:    "thinking",
			wantOmit:   false,
		},
		{
			name:       "omitted forces official thinking blocks and hides content",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "omitted"},
			defaultFmt: "think",
			wantFmt:    "thinking",
			wantOmit:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := resolveClaudeThinkingResponseOptions(tc.thinking, tc.defaultFmt)
			if opts.Format != tc.wantFmt {
				t.Fatalf("expected format %q, got %q", tc.wantFmt, opts.Format)
			}
			if opts.OmitDisplay != tc.wantOmit {
				t.Fatalf("expected omitDisplay=%v, got %v", tc.wantOmit, opts.OmitDisplay)
			}
		})
	}
}

func TestMergeUniqueModelsPreservesUnionAcrossAccounts(t *testing.T) {
	base := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"TEXT"}},
	}
	incoming := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"image"}},
		{ModelId: "claude-opus-4-7", InputTypes: []string{"text"}},
	}

	merged := mergeUniqueModels(base, incoming)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique models, got %d", len(merged))
	}
	if !modelSupportsImage(merged[0].InputTypes) {
		t.Fatalf("expected merged input types to preserve image capability, got %#v", merged[0].InputTypes)
	}
	if merged[1].ModelId != "claude-opus-4-7" {
		t.Fatalf("expected second model to be claude-opus-4-7, got %q", merged[1].ModelId)
	}
}

func TestBuildAnthropicModelsResponseGeneratesThinkingVariants(t *testing.T) {
	models := buildAnthropicModelsResponse([]ModelInfo{{
		ModelId:    "claude-sonnet-4.5",
		InputTypes: []string{"text", "image"},
	}}, "-thinking")

	if len(models) != 2 {
		t.Fatalf("expected base model and thinking variant, got %d", len(models))
	}
	if models[0]["id"] != "claude-sonnet-4.5" {
		t.Fatalf("unexpected base model id: %#v", models[0]["id"])
	}
	if models[1]["id"] != "claude-sonnet-4.5-thinking" {
		t.Fatalf("unexpected thinking model id: %#v", models[1]["id"])
	}
	if supportsImage, ok := models[0]["supports_image"].(bool); !ok || !supportsImage {
		t.Fatalf("expected image capability to be preserved, got %#v", models[0]["supports_image"])
	}
}

// TestFindDedupTargetExternalIdpDoesNotClobberOtherUser is the core regression
// for the "old account disappears on login" bug: external_idp (Azure AD) users
// in the same AWS org share one Q Developer profile ARN, so deduping by ARN
// alone would overwrite a different user's account. findDedupTarget must require
// a matching email for external_idp, and must never match when email is empty.
func TestFindDedupTargetExternalIdpDoesNotClobberOtherUser(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	const sharedArn = "arn:aws:codewhisperer:us-east-1:0:profile/ORG_SHARED"
	if err := config.AddAccount(config.Account{
		ID: "old", Enabled: true, AuthMethod: "external_idp",
		Email: "alice@corp.com", ProfileArn: sharedArn,
	}); err != nil {
		t.Fatalf("add old account: %v", err)
	}

	// A different org user logging in (same shared ARN, different email) must NOT
	// match the existing account — it should be appended instead of overwritten.
	if got := findDedupTarget(sharedArn, "bob@corp.com", "external_idp"); got != nil {
		t.Fatalf("external_idp dedup clobbered a different user: matched %q", got.ID)
	}

	// Empty email (JWT email unresolved) must also never match — appending a new
	// account is safer than overwriting an unrelated one.
	if got := findDedupTarget(sharedArn, "", "external_idp"); got != nil {
		t.Fatalf("external_idp dedup matched on empty email: matched %q", got.ID)
	}

	// Re-login of the SAME user (same ARN + same email) MUST match, so tokens are
	// updated in place rather than creating a duplicate.
	got := findDedupTarget(sharedArn, "alice@corp.com", "external_idp")
	if got == nil || got.ID != "old" {
		t.Fatalf("expected re-login of same user to match account old, got %#v", got)
	}
}

// TestFindDedupTargetIdcMatchesByProfileArnOnly verifies non-external_idp auth
// methods keep the original profileArn-only dedup behaviour (unchanged).
func TestFindDedupTargetIdcMatchesByProfileArnOnly(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	const arn = "arn:aws:codewhisperer:us-east-1:0:profile/IDC_USER"
	if err := config.AddAccount(config.Account{
		ID: "idc1", Enabled: true, AuthMethod: "idc",
		Email: "user@corp.com", ProfileArn: arn,
	}); err != nil {
		t.Fatalf("add idc account: %v", err)
	}

	// idc dedups on ARN alone — a differing/empty email still matches.
	if got := findDedupTarget(arn, "", "idc"); got == nil || got.ID != "idc1" {
		t.Fatalf("expected idc dedup to match by ARN regardless of email, got %#v", got)
	}
}

func TestUpstreamErrorStatus(t *testing.T) {
	cases := []struct {
		err  string
		want int
	}{
		{"HTTP 503 from claude-opus-4.8: {\"error\":{\"message\":\"system cpu overloaded (current: 100.0%, threshold: 90%)\"}}", 503},
		{"HTTP 502 from upstream: bad gateway", 503},
		{"HTTP 504 from upstream: gateway timeout", 503},
		{"The AI service is temporarily overloaded. Please try again in a moment.", 503},
		{"service unavailable", 503},
		{"rate limit exceeded", 503},
		{"quota exhausted", 503},
		{"too many requests", 503},
		{"dial tcp: lookup q.profile: no such host", 500},
		{"invalid model id", 500},
		{"unexpected end of JSON", 500},
	}
	for _, c := range cases {
		got := upstreamErrorStatus(errors.New(c.err))
		if got != c.want {
			t.Fatalf("upstreamErrorStatus(%q) = %d, want %d", c.err, got, c.want)
		}
	}
	if upstreamErrorStatus(nil) != 500 {
		t.Fatalf("upstreamErrorStatus(nil) = %d, want 500", upstreamErrorStatus(nil))
	}
}

// ── upsertYAMLModelSection tests ─────────────────────────────────────

func TestUpsertYAMLModelSectionExistingOmniroute(t *testing.T) {
	// Simulates the hitokiri scenario: existing config with provider: omniroute
	input := `model:
  default: gpt-5.6-terra
  provider: omniroute
  api_key: sk-old-key
  base_url: http://localhost:20128/v1
  api_mode: chat_completions
  context_length: 272000
  max_tokens: 128000
providers:
  omniroute:
    base_url: http://localhost:20128/v1
`
	out := upsertYAMLModelSection(input, "gpt-5.6-sol", "omniproxy", "http://localhost:20131/v1", "sk-new-key")

	// Provider should be omniproxy
	if !strings.Contains(out, "provider: omniproxy") {
		t.Errorf("provider should be omniproxy, got:\n%s", out)
	}
	// Default model should be updated
	if !strings.Contains(out, "default: gpt-5.6-sol") {
		t.Errorf("default should be gpt-5.6-sol, got:\n%s", out)
	}
	// base_url should be updated
	if !strings.Contains(out, "base_url: \"http://localhost:20131/v1\"") {
		t.Errorf("base_url should be updated, got:\n%s", out)
	}
	// api_key should be updated
	if !strings.Contains(out, "api_key: sk-new-key") {
		t.Errorf("api_key should be updated, got:\n%s", out)
	}
	// api_mode should be openai
	if !strings.Contains(out, "api_mode: openai") {
		t.Errorf("api_mode should be openai, got:\n%s", out)
	}
	// context_length and max_tokens should be preserved
	if !strings.Contains(out, "context_length: 272000") {
		t.Errorf("context_length should be preserved, got:\n%s", out)
	}
	if !strings.Contains(out, "max_tokens: 128000") {
		t.Errorf("max_tokens should be preserved, got:\n%s", out)
	}
	// Old omniroute provider should NOT be in model: section
	if strings.Contains(out, "provider: omniroute") {
		t.Errorf("old provider omniroute should be replaced, got:\n%s", out)
	}
	// providers: section should still be there
	if !strings.Contains(out, "providers:") {
		t.Errorf("providers: section should be preserved, got:\n%s", out)
	}
}

func TestUpsertYAMLModelSectionEmpty(t *testing.T) {
	out := upsertYAMLModelSection("", "gpt-5.6-sol", "omniproxy", "http://localhost:20131/v1", "sk-key")
	if !strings.Contains(out, "model:") {
		t.Errorf("should create model: section, got:\n%s", out)
	}
	if !strings.Contains(out, "provider: omniproxy") {
		t.Errorf("provider should be omniproxy, got:\n%s", out)
	}
}

func TestUpsertYAMLModelSectionNoModelSection(t *testing.T) {
	input := `providers:
  omniroute:
    base_url: http://localhost:20128/v1
`
	out := upsertYAMLModelSection(input, "gpt-5.6-sol", "omniproxy", "http://localhost:20131/v1", "sk-key")
	// model: section should be inserted at top
	if !strings.HasPrefix(out, "model:") {
		t.Errorf("model: should be at top, got:\n%s", out)
	}
	if !strings.Contains(out, "provider: omniproxy") {
		t.Errorf("provider should be omniproxy, got:\n%s", out)
	}
	// providers: section should still be there
	if !strings.Contains(out, "providers:") {
		t.Errorf("providers: should be preserved, got:\n%s", out)
	}
}

func TestUpsertYAMLModelSectionIdempotent(t *testing.T) {
	input := `model:
  default: gpt-5.6-sol
  provider: omniproxy
  base_url: "http://localhost:20131/v1"
  api_key: sk-key
  api_mode: openai
`
	out1 := upsertYAMLModelSection(input, "gpt-5.6-sol", "omniproxy", "http://localhost:20131/v1", "sk-key")
	out2 := upsertYAMLModelSection(out1, "gpt-5.6-sol", "omniproxy", "http://localhost:20131/v1", "sk-key")
	if out1 != out2 {
		t.Errorf("upsert should be idempotent\n--- first ---\n%s\n--- second ---\n%s", out1, out2)
	}
}
