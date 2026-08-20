package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"reflect"
	"strings"
	"testing"
)

// initConfigForTests initialises the global config singleton so code paths
// that touch config.GetProxyURL() / GetThinkingConfig() don't nil-deref.
func initConfigForTests(t *testing.T) {
	t.Helper()
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

// TestKiroPayloadToOpenAIRequestCollapseSystemPriming verifies that the
// system-prompt priming pair injected by OpenAIToKiro/ClaudeToKiro (user
// system prompt → assistant "I will follow...") is collapsed back into a
// single system message for the external provider.
func TestKiroPayloadToOpenAIRequestCollapseSystemPriming(t *testing.T) {
	initConfigForTests(t)
	req := &OpenAIRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMessage{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hello"},
		},
	}
	payload := OpenAIToKiro(req, false)
	payload.OriginalModel = "gpt-4o"

	body, err := kiroPayloadToOpenAIRequest(payload, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	msgs, ok := body["messages"].([]map[string]interface{})
	if !ok {
		t.Fatalf("messages not a slice: %T", body["messages"])
	}
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message role = %v, want system", msgs[0]["role"])
	}
	if !strings.Contains(fmt.Sprint(msgs[0]["content"]), "You are helpful.") {
		t.Fatalf("system content lost: %v", msgs[0]["content"])
	}
	// The assistant priming "I will follow..." must NOT appear as a separate
	// message — it should have been collapsed into the system message.
	for _, m := range msgs {
		if m["role"] == "assistant" {
			t.Fatalf("unexpected assistant priming message: %v", m)
		}
	}
	// Final user message must be present.
	last := msgs[len(msgs)-1]
	if last["role"] != "user" || !strings.Contains(fmt.Sprint(last["content"]), "Hello") {
		t.Fatalf("last message = %v, want user Hello", last)
	}
	if body["model"] != "gpt-4o" {
		t.Fatalf("model = %v, want gpt-4o", body["model"])
	}
	if body["stream"] != nil {
		t.Fatalf("stream should not be set by converter (caller sets it): %v", body["stream"])
	}
}

func TestKiroPayloadToOpenAIRequestPreservesClaudeImages(t *testing.T) {
	initConfigForTests(t)
	const imageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	imageContent := func(text string) []interface{} {
		return []interface{}{
			map[string]interface{}{"type": "text", "text": text},
			map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type":       "base64",
					"media_type": "image/png",
					"data":       imageData,
				},
			},
		}
	}
	req := &ClaudeRequest{
		Model: "gpt-4o",
		Messages: []ClaudeMessage{
			{Role: "user", Content: imageContent("History image")},
			{Role: "assistant", Content: "I see it."},
			{Role: "user", Content: imageContent("Current image")},
		},
	}
	payload := ClaudeToKiro(req, false)
	payload.OriginalModel = "gpt-4o"
	if len(payload.ConversationState.History[0].UserInputMessage.Images) != 1 {
		t.Fatalf("ClaudeToKiro history image count = %d, want 1", len(payload.ConversationState.History[0].UserInputMessage.Images))
	}
	if len(payload.ConversationState.CurrentMessage.UserInputMessage.Images) != 1 {
		t.Fatalf("ClaudeToKiro current image count = %d, want 1", len(payload.ConversationState.CurrentMessage.UserInputMessage.Images))
	}

	body, err := kiroPayloadToOpenAIRequest(payload, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	messages := body["messages"].([]map[string]interface{})
	assertImageContent := func(message map[string]interface{}, text string) {
		t.Helper()
		content, ok := message["content"].([]map[string]interface{})
		if !ok {
			t.Fatalf("user content = %T, want multimodal content blocks", message["content"])
		}
		if len(content) != 2 || content[0]["type"] != "text" || content[0]["text"] != text {
			t.Fatalf("unexpected text content block: %#v", content)
		}
		imageURL, ok := content[1]["image_url"].(map[string]interface{})
		if !ok || content[1]["type"] != "image_url" {
			t.Fatalf("unexpected image content block: %#v", content[1])
		}
		wantURL := "data:image/png;base64," + imageData
		if imageURL["url"] != wantURL {
			t.Fatalf("image URL = %v, want %v", imageURL["url"], wantURL)
		}
	}
	assertImageContent(messages[0], "History image")
	assertImageContent(messages[len(messages)-1], "Current image")
}

// TestKiroPayloadToOpenAIRequestRestoresToolNames verifies sanitized tool names
// are restored to originals via ToolNameMap before being sent to the external
// provider (which has no Kiro tool-name restrictions).

func TestKiroPayloadToOpenAIRequestRestoresToolNames(t *testing.T) {
	initConfigForTests(t)
	// Use a name >64 chars so shortenToolName actually sanitizes it; otherwise
	// the original name passes through unchanged and ToolNameMap stays empty.
	originalName := "my-very-cool-tool-with-a-name-that-exceeds-the-kiro-64-char-limit-yes-really"
	req := &OpenAIRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMessage{{Role: "user", Content: "use the tool"}},
		Tools: []OpenAITool{{
			Type: "function",
			Function: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Parameters  interface{} `json:"parameters"`
			}{Name: originalName, Description: "does a thing", Parameters: map[string]interface{}{"type": "object"}},
		}},
	}
	payload := OpenAIToKiro(req, false)
	// ToolNameMap should map the sanitized name back to the original long name.
	if len(payload.ToolNameMap) == 0 {
		t.Fatal("expected ToolNameMap to be populated by OpenAIToKiro")
	}

	body, err := kiroPayloadToOpenAIRequest(payload, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	tools, ok := body["tools"].([]map[string]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v (%T)", body["tools"], body["tools"])
	}
	fn, _ := tools[0]["function"].(map[string]interface{})
	name, _ := fn["name"].(string)
	if name != originalName {
		t.Fatalf("tool name = %q, want %q (restored)", name, originalName)
	}
}

func TestKiroPayloadToOpenAIRequestRemovesNonStringToolEnums(t *testing.T) {
	initConfigForTests(t)

	parameters := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"enabled": map[string]interface{}{
				"type": "boolean",
				"enum": []interface{}{false},
			},
			"count": map[string]interface{}{
				"type": "integer",
				"enum": []interface{}{float64(1)},
			},
			"mode": map[string]interface{}{
				"type": "string",
				"enum": []interface{}{"fast", "safe"},
			},
			"value": map[string]interface{}{
				"anyOf": []interface{}{
					map[string]interface{}{
						"type": "boolean",
						"enum": []interface{}{false},
					},
					map[string]interface{}{"type": "null"},
				},
			},
		},
	}
	req := &OpenAIRequest{
		Model:    "google/gemini-test",
		Messages: []OpenAIMessage{{Role: "user", Content: "use the tool"}},
		Tools: []OpenAITool{{
			Type: "function",
			Function: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Parameters  interface{} `json:"parameters"`
			}{Name: "configure", Description: "Configure a value", Parameters: parameters},
		}},
	}
	payload := OpenAIToKiro(req, false)
	payload.OriginalModel = req.Model

	body, err := kiroPayloadToOpenAIRequest(payload, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	tools := body["tools"].([]map[string]interface{})
	function := tools[0]["function"].(map[string]interface{})
	schema := function["parameters"].(map[string]interface{})
	properties := schema["properties"].(map[string]interface{})

	for _, name := range []string{"enabled", "count"} {
		property := properties[name].(map[string]interface{})
		if _, exists := property["enum"]; exists {
			t.Fatalf("non-string enum for %s was not removed: %#v", name, property)
		}
	}
	valueAnyOf := properties["value"].(map[string]interface{})["anyOf"].([]interface{})
	if _, exists := valueAnyOf[0].(map[string]interface{})["enum"]; exists {
		t.Fatalf("nested non-string enum was not removed: %#v", valueAnyOf[0])
	}
	if got := properties["mode"].(map[string]interface{})["enum"]; !reflect.DeepEqual(got, []interface{}{"fast", "safe"}) {
		t.Fatalf("valid string enum changed: %#v", got)
	}

	originalEnabled := parameters["properties"].(map[string]interface{})["enabled"].(map[string]interface{})
	if _, exists := originalEnabled["enum"]; !exists {
		t.Fatal("outbound sanitization mutated the caller's schema")
	}
}

func TestClaudeToolChoiceReachesExternalOpenAIRequest(t *testing.T) {
	initConfigForTests(t)

	tool := ClaudeTool{
		Name:        "Read",
		Description: "Read a file",
		InputSchema: map[string]interface{}{"type": "object"},
	}
	choices := []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{name: "any", in: map[string]interface{}{"type": "any"}, want: "required"},
		{name: "none", in: map[string]interface{}{"type": "none"}, want: "none"},
		{name: "auto", in: map[string]interface{}{"type": "auto"}, want: "auto"},
		{
			name: "specific tool",
			in:   map[string]interface{}{"type": "tool", "name": "Read"},
			want: map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": "Read"},
			},
		},
	}

	for _, tc := range choices {
		t.Run(tc.name, func(t *testing.T) {
			payload := ClaudeToKiro(&ClaudeRequest{
				Model:      "claude-opus-5",
				Messages:   []ClaudeMessage{{Role: "user", Content: "inspect the file"}},
				Tools:      []ClaudeTool{tool},
				ToolChoice: tc.in,
			}, false)
			payload.OriginalModel = "claude-opus-5"

			body, err := kiroPayloadToOpenAIRequest(payload, nil)
			if err != nil {
				t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
			}
			if got := body["tool_choice"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tool_choice = %#v, want %#v", got, tc.want)
			}
		})
	}

	noChoice := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-opus-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "answer normally"}},
		Tools:    []ClaudeTool{tool},
	}, false)
	body, err := kiroPayloadToOpenAIRequest(noChoice, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest without choice: %v", err)
	}
	if _, ok := body["tool_choice"]; ok {
		t.Fatalf("tool_choice should be omitted when the client did not specify one: %#v", body["tool_choice"])
	}
}

// TestCallExternalOpenAISSEStream verifies the SSE parser replays content,
// reasoning, tool_calls, and usage through the KiroStreamCallback.
func TestCallExternalOpenAISSEStream(t *testing.T) {
	initConfigForTests(t)
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"kiro\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	modelsRequests := 0
	var requestedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelsRequests++
			http.Error(w, "model discovery must not run during inference", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q, want Bearer sk-test", got)
		}
		var requestBody struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode chat request: %v", err)
		}
		requestedModel = requestBody.Model
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	defer srv.Close()

	account := &config.Account{
		ID:          "ext-1",
		Email:       "test-provider",
		AuthMethod:  externalAuthMethod,
		AccessToken: "sk-test",
		BaseURL:     srv.URL,
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)
	// OpenAI handlers preserve the client-supplied model separately from the
	// Kiro routing alias produced by OpenAIToKiro. Mirror that production step
	// so this test verifies the external request model, not the internal alias.
	payload.OriginalModel = "gpt-4o"

	var text, reasoning string
	var tools []KiroToolUse
	var inTok, outTok int
	cb := &KiroStreamCallback{
		OnText: func(s string, isThinking bool) {
			if isThinking {
				reasoning += s
			} else {
				text += s
			}
		},
		OnToolUse:  func(tu KiroToolUse) { tools = append(tools, tu) },
		OnComplete: func(i, o int) { inTok, outTok = i, o },
	}
	if err := CallExternalOpenAI(account, payload, cb); err != nil {
		t.Fatalf("CallExternalOpenAI: %v", err)
	}
	if text != "Hello" {
		t.Errorf("text = %q, want Hello", text)
	}
	if reasoning != "thinking..." {
		t.Errorf("reasoning = %q, want thinking...", reasoning)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].Name != "search" || tools[0].ToolUseID != "call_1" {
		t.Errorf("tool = %+v", tools[0])
	}
	if q, _ := tools[0].Input["q"].(string); q != "kiro" {
		t.Errorf("tool input = %+v, want q=kiro", tools[0].Input)
	}
	if inTok != 12 || outTok != 7 {
		t.Errorf("tokens = in=%d out=%d, want in=12 out=7", inTok, outTok)
	}
	if modelsRequests != 0 {
		t.Fatalf("/v1/models requests = %d, want 0 during inference", modelsRequests)
	}
	if requestedModel != "gpt-4o" {
		t.Fatalf("upstream model = %q, want gpt-4o", requestedModel)
	}
}

func TestParseExternalOpenAISSERejectsEmptyTerminalStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":0}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var text string
	completeCalls := 0
	err := parseExternalOpenAISSE(strings.NewReader(sse), &KiroStreamCallback{
		OnText: func(chunk string, _ bool) { text += chunk },
		OnComplete: func(_, _ int) {
			completeCalls++
		},
	})
	if err == nil {
		t.Fatal("expected empty terminal stream to fail")
	}
	if !strings.Contains(err.Error(), "without assistant output") {
		t.Fatalf("error = %q, want missing assistant output", err)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if completeCalls != 0 {
		t.Fatalf("OnComplete calls = %d, want 0", completeCalls)
	}
}

func TestParseExternalOpenAISSEAcceptsToolDeltaWithoutContent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":4}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var tools []KiroToolUse
	completeCalls := 0
	outputSignals := 0
	err := parseExternalOpenAISSE(strings.NewReader(sse), &KiroStreamCallback{
		OnOutput: func() { outputSignals++ },
		OnToolUse: func(tool KiroToolUse) {
			tools = append(tools, tool)
		},
		OnComplete: func(_, _ int) {
			completeCalls++
		},
	})
	if err != nil {
		t.Fatalf("parseExternalOpenAISSE: %v", err)
	}
	if outputSignals != 1 {
		t.Fatalf("OnOutput calls = %d, want 1", outputSignals)
	}
	if len(tools) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(tools))
	}
	if tools[0].ToolUseID != "call_read" || tools[0].Name != "Read" {
		t.Fatalf("tool = %+v", tools[0])
	}
	if got := tools[0].Input["path"]; got != "README.md" {
		t.Fatalf("tool path = %v, want README.md", got)
	}
	if completeCalls != 1 {
		t.Fatalf("OnComplete calls = %d, want 1", completeCalls)
	}
}

// TestCallExternalOpenAIJSONFallback verifies a provider that ignores
// stream=true and returns a single JSON object is still parsed correctly.
func TestCallExternalOpenAIJSONFallback(t *testing.T) {
	initConfigForTests(t)
	modelsRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelsRequests++
			http.Error(w, "model discovery must not run during inference", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi there","tool_calls":[]}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer srv.Close()

	account := &config.Account{
		ID: "ext-2", Email: "json-provider", AuthMethod: externalAuthMethod,
		AccessToken: "sk-test", BaseURL: srv.URL,
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)

	var text string
	var inTok, outTok int
	cb := &KiroStreamCallback{
		OnText:     func(s string, _ bool) { text += s },
		OnComplete: func(i, o int) { inTok, outTok = i, o },
	}
	if err := CallExternalOpenAI(account, payload, cb); err != nil {
		t.Fatalf("CallExternalOpenAI: %v", err)
	}
	if text != "hi there" {
		t.Errorf("text = %q, want hi there", text)
	}
	if inTok != 3 || outTok != 2 {
		t.Errorf("tokens = in=%d out=%d, want in=3 out=2", inTok, outTok)
	}
	if modelsRequests != 0 {
		t.Fatalf("/v1/models requests = %d, want 0 during inference", modelsRequests)
	}
}

// TestCallExternalOpenAIAuthError verifies 401 is surfaced (so the pool can
// disable the account) rather than swallowed.
func TestCallExternalOpenAIAuthError(t *testing.T) {
	initConfigForTests(t)
	modelsRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelsRequests++
			http.Error(w, "model discovery must not run during inference", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"invalid api key"}`)
	}))
	defer srv.Close()

	account := &config.Account{
		ID: "ext-3", Email: "bad-key", AuthMethod: externalAuthMethod,
		AccessToken: "sk-bad", BaseURL: srv.URL,
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "gpt-4o",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)

	err := CallExternalOpenAI(account, payload, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want 401", err)
	}
	if modelsRequests != 0 {
		t.Fatalf("/v1/models requests = %d, want 0 during inference", modelsRequests)
	}
}

// TestIsExternalAccount verifies the auth-method guard.
func TestIsExternalAccount(t *testing.T) {
	if isExternalAccount(&config.Account{AuthMethod: "idc"}) {
		t.Fatal("idc should not be external")
	}
	if !isExternalAccount(&config.Account{AuthMethod: externalAuthMethod}) {
		t.Fatal("external_openai should be external")
	}
	if isExternalAccount(nil) {
		t.Fatal("nil should not be external")
	}
}

// TestStripInternalModelPrefix verifies that internal routing prefixes
// ("kr/", "omniproxy/") are stripped before sending to external providers,
// while bare model IDs pass through unchanged.
func TestStripInternalModelPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"kr/claude-sonnet-5", "claude-sonnet-5"},
		{"omniproxy/claude-sonnet-5", "claude-sonnet-5"},
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"gpt-4o", "gpt-4o"},
		{"kr/auto", "auto"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripInternalModelPrefix(c.in); got != c.want {
			t.Errorf("stripInternalModelPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDotToDashClaudeVersion verifies that dot-form claude version IDs
// (OmniProxy's internal normalisation) are reverted to dash-form for
// external providers that use dash-form model registries (e.g. bddevlab).
func TestDotToDashClaudeVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-opus-4.8", "claude-opus-4-8"},
		{"claude-sonnet-4.5", "claude-sonnet-4-5"},
		{"claude-haiku-4.5", "claude-haiku-4-5"},
		{"claude-sonnet-5", "claude-sonnet-5"},                   // no dot → unchanged
		{"claude-opus-4-8", "claude-opus-4-8"},                   // already dash → unchanged
		{"gpt-4o", "gpt-4o"},                                     // non-claude → unchanged
		{"claude-sonnet-4-20250514", "claude-sonnet-4-20250514"}, // dated snapshot → unchanged
	}
	for _, c := range cases {
		if got := dotToDashClaudeVersion(c.in); got != c.want {
			t.Errorf("dotToDashClaudeVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKiroPayloadToOpenAIRequestAppliesAccountModelMapping(t *testing.T) {
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:    "claude-haiku-5",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)
	payload.OriginalModel = "claude-haiku-5"
	account := &config.Account{ModelMappings: map[string]string{
		"CLAUDE-HAIKU-5": "claude-sonnet-5",
	}}

	body, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	if got := body["model"]; got != "claude-sonnet-5" {
		t.Fatalf("outbound model = %v, want claude-sonnet-5", got)
	}
	if payload.OriginalModel != "claude-haiku-5" {
		t.Fatalf("public payload model changed to %q", payload.OriginalModel)
	}
}
