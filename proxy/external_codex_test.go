package proxy

import (
	"encoding/json"
	"fmt"
	"omniproxy/config"
	"path/filepath"
	"reflect"
	"strings"
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

func TestCodexSubscriptionModelsUsesEvidenceBackedGPT56Limits(t *testing.T) {
	want := map[string]struct {
		input  int
		output int
	}{
		"gpt-5.6":       {input: 272_000, output: 128_000},
		"gpt-5.6-sol":   {input: 272_000, output: 128_000},
		"gpt-5.6-terra": {input: 272_000, output: 128_000},
		"gpt-5.6-luna":  {input: 272_000, output: 128_000},
	}

	for _, model := range codexSubscriptionModels() {
		limits, ok := want[model.ModelId]
		if !ok {
			continue
		}
		if model.TokenLimits == nil {
			t.Fatalf("model %s missing TokenLimits", model.ModelId)
		}
		if got := model.TokenLimits.MaxInputTokens; got != limits.input {
			t.Errorf("model %s MaxInputTokens = %d, want %d", model.ModelId, got, limits.input)
		}
		if got := model.TokenLimits.MaxOutputTokens; got != limits.output {
			t.Errorf("model %s MaxOutputTokens = %d, want %d", model.ModelId, got, limits.output)
		}
		delete(want, model.ModelId)
	}

	for id := range want {
		t.Errorf("codexSubscriptionModels missing %q", id)
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

func TestMarkCodexReauthRequiredPersistsOnlyAuthenticationFailures(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	reauthAccount := config.Account{
		ID:          "codex-reauth",
		Email:       "reauth@example.test",
		AuthMethod:  codexAuthMethod,
		AccessToken: "access-token",
		Enabled:     true,
	}
	if err := config.AddAccount(reauthAccount); err != nil {
		t.Fatalf("add reauth account: %v", err)
	}
	markCodexReauthRequired(&reauthAccount, fmt.Errorf("Codex token refresh: HTTP 401: invalid_grant"))

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	got := accounts[0]
	if got.BanStatus != codexReauthRequiredStatus || got.Enabled {
		t.Fatalf("401 account status = %q enabled=%v, want %q and disabled", got.BanStatus, got.Enabled, codexReauthRequiredStatus)
	}
	if !strings.Contains(got.BanReason, "401") {
		t.Fatalf("reauth reason = %q, want original 401 error", got.BanReason)
	}

	rateLimited := config.Account{
		ID:          "codex-rate-limited",
		Email:       "rate@example.test",
		AuthMethod:  codexAuthMethod,
		AccessToken: "access-token",
		Enabled:     true,
	}
	if err := config.AddAccount(rateLimited); err != nil {
		t.Fatalf("add rate-limited account: %v", err)
	}
	markCodexReauthRequired(&rateLimited, fmt.Errorf("Codex usage fetch: HTTP 429 rate limit"))

	accounts = config.GetAccounts()
	for _, account := range accounts {
		if account.ID != rateLimited.ID {
			continue
		}
		if account.BanStatus == codexReauthRequiredStatus || !account.Enabled {
			t.Fatalf("429 account was incorrectly marked for re-login: %+v", account)
		}
		return
	}
	t.Fatal("rate-limited account missing")
}

func TestRestoreCallbackToolNames(t *testing.T) {
	canonicalNames := []string{"Skill", "Bash", "Read", "Agent", "EnterPlanMode"}
	tools := make([]ClaudeTool, 0, len(canonicalNames))
	for _, name := range canonicalNames {
		tools = append(tools, ClaudeTool{
			Name:        name,
			Description: "Test tool " + name,
			InputSchema: map[string]interface{}{"type": "object"},
		})
	}
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:    "gpt-5.6-sol",
		Messages: []ClaudeMessage{{Role: "user", Content: "Use a tool"}},
		Tools:    tools,
	}, false)

	sanitizedNames := []string{"skill", "bash", "read", "agent", "enterPlanMode"}
	for i, sanitized := range sanitizedNames {
		if got := payload.ToolNameMap[sanitized]; got != canonicalNames[i] {
			t.Fatalf("ToolNameMap[%q] = %q, want %q", sanitized, got, canonicalNames[i])
		}
	}

	var got []string
	callback := restoreCallbackToolNames(payload, &KiroStreamCallback{
		OnToolUse: func(tu KiroToolUse) {
			got = append(got, tu.Name)
		},
	})
	for _, name := range append(sanitizedNames, "unknownTool") {
		callback.OnToolUse(KiroToolUse{Name: name})
	}

	want := append(canonicalNames, "unknownTool")
	if len(got) != len(want) {
		t.Fatalf("got %d tool calls, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRestoreCallbackToolNamesPreservesNilAndOtherCallbacks(t *testing.T) {
	if got := restoreCallbackToolNames(&KiroPayload{}, nil); got != nil {
		t.Fatal("nil callback should remain nil")
	}

	completed := false
	original := &KiroStreamCallback{OnComplete: func(_, _ int) { completed = true }}
	got := restoreCallbackToolNames(&KiroPayload{ToolNameMap: map[string]string{"read": "Read"}}, original)
	if got != original {
		t.Fatal("callback without OnToolUse should not be wrapped")
	}
	got.OnComplete(0, 0)
	if !completed {
		t.Fatal("non-tool callbacks must be preserved")
	}
}

func TestCodexResponsesAddsTaskStopLifecycleGuidance(t *testing.T) {
	payload := &KiroPayload{ToolNameMap: map[string]string{"taskStop": "TaskStop"}}
	ctx := &UserInputMessageContext{}
	for _, tool := range []struct {
		name string
		desc string
	}{
		{name: "taskStop", desc: "Stop a background task."},
		{name: "read", desc: "Read a file."},
	} {
		wrapper := KiroToolWrapper{}
		wrapper.ToolSpecification.Name = tool.name
		wrapper.ToolSpecification.Description = tool.desc
		wrapper.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{"type": "object"}}
		ctx.Tools = append(ctx.Tools, wrapper)
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = ctx

	body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
	if err != nil {
		t.Fatalf("convert Kiro payload: %v", err)
	}
	tools, ok := body["tools"].([]map[string]interface{})
	if !ok || len(tools) != 2 {
		t.Fatalf("unexpected tools: %#v", body["tools"])
	}
	if tools[0]["name"] != "TaskStop" {
		t.Fatalf("TaskStop name was not restored: %#v", tools[0])
	}
	stopDesc, _ := tools[0]["description"].(string)
	for _, phrase := range []string{"currently running", "terminal", "Never retry"} {
		if !strings.Contains(stopDesc, phrase) {
			t.Errorf("TaskStop description missing %q: %q", phrase, stopDesc)
		}
	}
	if got := tools[1]["description"]; got != "Read a file." {
		t.Errorf("non-TaskStop description changed: %q", got)
	}
}

func TestClaudeToolChoiceReachesCodexResponsesRequest(t *testing.T) {
	tool := ClaudeTool{
		Name:        "Read",
		Description: "Read a file",
		InputSchema: map[string]interface{}{"type": "object"},
	}
	tests := []struct {
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
			want: map[string]interface{}{"type": "function", "name": "Read"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := ClaudeToKiro(&ClaudeRequest{
				Model:      "gpt-5.6-sol",
				Messages:   []ClaudeMessage{{Role: "user", Content: "inspect the file"}},
				Tools:      []ClaudeTool{tool},
				ToolChoice: tc.in,
			}, false)
			payload.OriginalModel = "gpt-5.6-sol"

			body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
			if err != nil {
				t.Fatalf("kiroPayloadToCodexResponsesRequest: %v", err)
			}
			if got := body["tool_choice"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tool_choice = %#v, want %#v", got, tc.want)
			}
		})
	}

	noChoice := ClaudeToKiro(&ClaudeRequest{
		Model:    "gpt-5.6-sol",
		Messages: []ClaudeMessage{{Role: "user", Content: "answer normally"}},
		Tools:    []ClaudeTool{tool},
	}, false)
	body, err := kiroPayloadToCodexResponsesRequest(noChoice, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToCodexResponsesRequest without choice: %v", err)
	}
	if _, ok := body["tool_choice"]; ok {
		t.Fatalf("tool_choice should be omitted when the client did not specify one: %#v", body["tool_choice"])
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

func TestCodexResponsesToolImageIsForwardedOnce(t *testing.T) {
	const imageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":"inspect the image"},
		{"type":"function_call","call_id":"call_img","name":"view_image","arguments":"{\"path\":\"image.png\"}"},
		{"type":"function_call_output","call_id":"call_img","output":[
			{"type":"input_text","text":"screenshot"},
			{"type":"input_image","image_url":"data:image/png;base64,` + imageData + `"}
		]}
	]`)

	messages, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse Responses input: %v", err)
	}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "gpt-5.6-sol", Messages: messages}, false)
	body, err := kiroPayloadToCodexResponsesRequest(payload, nil)
	if err != nil {
		t.Fatalf("convert Kiro payload: %v", err)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Codex request: %v", err)
	}
	if got := strings.Count(string(encoded), imageData); got != 1 {
		t.Fatalf("expected image base64 exactly once in outgoing request, got %d", got)
	}

	input, ok := body["input"].([]map[string]interface{})
	if !ok {
		t.Fatalf("expected typed input array, got %T", body["input"])
	}
	if len(input) != 4 {
		t.Fatalf("expected user, function call, tool output, and image message; got %d items", len(input))
	}
	if input[1]["type"] != "function_call" || input[1]["call_id"] != "call_img" {
		t.Fatalf("unexpected function call item: %#v", input[1])
	}
	if input[2]["type"] != "function_call_output" || input[2]["call_id"] != "call_img" {
		t.Fatalf("unexpected function output item: %#v", input[2])
	}
	if output, _ := input[2]["output"].(string); strings.Contains(output, imageData) {
		t.Fatal("image base64 leaked into function_call_output text")
	}
	if input[3]["type"] != "message" {
		t.Fatalf("expected image message after tool output, got %#v", input[3])
	}
}
