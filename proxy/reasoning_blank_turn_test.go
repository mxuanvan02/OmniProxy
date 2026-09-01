package proxy

import (
	"strings"
	"testing"
)

// Claude Desktop sends thinking.type "adaptive" (the model picks its own
// budget) rather than "enabled". External OpenAI-compatible providers only
// allocate a reasoning budget when reasoning_effort is present, so adaptive
// must still resolve to an effort or every Desktop turn silently runs without
// reasoning.
func TestClaudeReasoningEffortCoversAdaptiveRequests(t *testing.T) {
	tests := []struct {
		name     string
		thinking *ClaudeThinkingConfig
		want     string
		wantOK   bool
	}{
		{name: "nil config requests no reasoning", thinking: nil, wantOK: false},
		{name: "disabled requests no reasoning", thinking: &ClaudeThinkingConfig{Type: "disabled"}, wantOK: false},
		{name: "adaptive without budget defaults to high", thinking: &ClaudeThinkingConfig{Type: "adaptive"}, want: "high", wantOK: true},
		{name: "adaptive honours explicit effort", thinking: &ClaudeThinkingConfig{Type: "adaptive", Effort: "xhigh"}, want: "xhigh", wantOK: true},
		{name: "explicit effort wins over budget", thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 1024, Effort: "max"}, want: "max", wantOK: true},
		{name: "unknown effort falls back to budget", thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 12000, Effort: "turbo"}, want: "high", wantOK: true},
		{name: "budget 50k maps to max", thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 50000}, want: "max", wantOK: true},
		{name: "budget 10k maps to high", thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 10000}, want: "high", wantOK: true},
		{name: "budget 3k maps to medium", thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 3000}, want: "medium", wantOK: true},
		{name: "small budget maps to low", thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 1024}, want: "low", wantOK: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := claudeReasoningEffort(tc.thinking)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("effort = %q, want %q", got, tc.want)
			}
		})
	}
}

// An adaptive request must reach the upstream body as reasoning_effort.
func TestAdaptiveThinkingReachesExternalRequestBody(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-opus-5",
		MaxTokens: 4096,
		Messages:  []ClaudeMessage{{Role: "user", Content: "review this repo"}},
		Thinking:  &ClaudeThinkingConfig{Type: "adaptive"},
	}
	payload := ClaudeToKiro(req, true)
	if payload.InferenceConfig == nil {
		t.Fatal("InferenceConfig is nil for an adaptive thinking request")
	}
	if got := payload.InferenceConfig.ReasoningEffort; got != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", got)
	}

	body, err := kiroPayloadToOpenAIRequest(payload, nil)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("body reasoning_effort = %v, want high", got)
	}
}

// A turn whose only content is whitespace must be reported as an error so the
// caller can rotate accounts, rather than reaching the client as a finished but
// empty answer. This is the shape observed in production: a lone space with
// finish_reason "length".
func TestParseExternalOpenAISSERejectsBlankTurn(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":" "}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"length"}],"usage":{"prompt_tokens":7593,"completion_tokens":50}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var text strings.Builder
	outputSignals, completeCalls, stopReasons := 0, 0, 0
	err := parseExternalOpenAISSE(strings.NewReader(sse), &KiroStreamCallback{
		OnOutput:     func() { outputSignals++ },
		OnText:       func(chunk string, _ bool) { text.WriteString(chunk) },
		OnComplete:   func(_, _ int) { completeCalls++ },
		OnStopReason: func(string) { stopReasons++ },
	})
	if err == nil {
		t.Fatal("blank turn was reported as success")
	}
	if !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("error = %v, want an output-limit description", err)
	}
	// Nothing may escape to the client: any emitted byte would make the attempt
	// unretryable and replay a prefix on the next account.
	if outputSignals != 0 {
		t.Fatalf("OnOutput calls = %d, want 0", outputSignals)
	}
	if got := text.String(); got != "" {
		t.Fatalf("forwarded text = %q, want empty", got)
	}
	if completeCalls != 0 || stopReasons != 0 {
		t.Fatalf("terminal callbacks fired (complete=%d, stop=%d), want 0", completeCalls, stopReasons)
	}
}

// Whitespace that precedes real content must still be delivered, in order, so
// gating cannot corrupt formatting such as a leading indent.
func TestParseExternalOpenAISSEPreservesLeadingWhitespace(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"  "}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"real answer"}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var text strings.Builder
	stopReason := ""
	err := parseExternalOpenAISSE(strings.NewReader(sse), &KiroStreamCallback{
		OnText:       func(chunk string, _ bool) { text.WriteString(chunk) },
		OnStopReason: func(reason string) { stopReason = reason },
	})
	if err != nil {
		t.Fatalf("parseExternalOpenAISSE: %v", err)
	}
	if got := text.String(); got != "  real answer" {
		t.Fatalf("text = %q, want %q", got, "  real answer")
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", stopReason)
	}
}

// A blank turn that only carries reasoning is still blank to the user.
func TestParseExternalOpenAISSERejectsReasoningOnlyBlankTurn(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":" "}}]}`,
		``,
		`data: {"choices":[{"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	err := parseExternalOpenAISSE(strings.NewReader(sse), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("reasoning-only blank turn was reported as success")
	}
	if !strings.Contains(err.Error(), "blank assistant output") {
		t.Fatalf("error = %v, want a blank-output description", err)
	}
}

// The non-stream path (providers that ignore stream=true) needs the same guard.
func TestParseExternalOpenAIJSONRejectsBlankTurn(t *testing.T) {
	body := `{"choices":[{"message":{"content":" "},"finish_reason":"length"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
	err := parseExternalOpenAIJSON(strings.NewReader(body), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("blank JSON turn was reported as success")
	}
	if !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("error = %v, want an output-limit description", err)
	}
}

// blankTurnError must stay digit-free: error classification matches bare status
// tokens anywhere in the message, so an embedded token count could be read as an
// upstream 401/403/503 and trigger an unrelated refresh, cooldown, or ban.
func TestBlankTurnErrorIsNotMisclassified(t *testing.T) {
	for _, stopReason := range []string{"max_tokens", "end_turn"} {
		msg := blankTurnError(stopReason).Error()
		if strings.ContainsAny(msg, "0123456789") {
			t.Fatalf("blank-turn error contains digits and can be misread as a status code: %q", msg)
		}
		if isAuthErrorMessage(msg) {
			t.Fatalf("blank-turn error classified as an auth failure: %q", msg)
		}
		if isQuotaErrorMessage(msg) {
			t.Fatalf("blank-turn error classified as a quota failure: %q", msg)
		}
		if isSuspensionErrorMessage(msg) {
			t.Fatalf("blank-turn error classified as a suspension: %q", msg)
		}
	}
}
