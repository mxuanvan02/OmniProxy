package proxy

import (
	"testing"

	"omniproxy/config"
)

func TestApplyAntigravityCodingFilter(t *testing.T) {
	const sys = "You are Claude Code, Anthropic's official CLI. You may also use Cursor."

	tests := []struct {
		name        string
		mode        string
		want        string
		wantBlocked bool
	}{
		{
			name: "off is a no-op",
			mode: "off",
			want: sys,
		},
		{
			name: "empty mode is a no-op",
			mode: "",
			want: sys,
		},
		{
			name: "rewrite replaces every matched name",
			mode: "rewrite",
			want: "You are Antigravity, Anthropic's official CLI. You may also use Antigravity.",
		},
		{
			name:        "block flags a matched name",
			mode:        "block",
			want:        sys,
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, blocked := applyAntigravityCodingFilter(tt.mode, sys)
			if got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
			if blocked != tt.wantBlocked {
				t.Errorf("blocked = %v, want %v", blocked, tt.wantBlocked)
			}
		})
	}
}

func TestApplyAntigravityCodingFilterNoMatch(t *testing.T) {
	const clean = "You are a helpful assistant that writes Go."
	for _, mode := range []string{"rewrite", "block"} {
		got, blocked := applyAntigravityCodingFilter(mode, clean)
		if got != clean {
			t.Errorf("mode %q: output = %q, want unchanged", mode, got)
		}
		if blocked {
			t.Errorf("mode %q: blocked on clean prompt", mode)
		}
	}
}

func TestAntigravityFilterLongestMatchFirst(t *testing.T) {
	// "Codex CLI" must win over the shorter "Codex".
	got, _ := applyAntigravityCodingFilter("rewrite", "using Codex CLI here")
	if got != "using Antigravity here" {
		t.Errorf("output = %q, want %q", got, "using Antigravity here")
	}
}

// End-to-end through the real request builder: the account's filter setting
// must scrub / block the systemInstruction that reaches Cloud Code Assist.
func filterPayload() *KiroPayload {
	p := &KiroPayload{OriginalModel: "gemini-3-pro-high"}
	p.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "You are Claude Code, Anthropic's official CLI."}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
	}
	p.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{Content: "Hi"}
	return p
}

func systemText(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	inner := body["request"].(map[string]interface{})
	sys, ok := inner["systemInstruction"].(map[string]interface{})
	if !ok {
		return ""
	}
	parts := sys["parts"].([]map[string]interface{})
	return parts[0]["text"].(string)
}

func TestAntigravityRequestFilterRewrite(t *testing.T) {
	acc := &config.Account{AntigravityCodingFilter: "rewrite"}
	body, err := kiroPayloadToAntigravityRequest(filterPayload(), acc, "proj-1")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	got := systemText(t, body)
	want := "You are Antigravity, Anthropic's official CLI."
	if got != want {
		t.Errorf("systemInstruction = %q, want %q", got, want)
	}
}

func TestAntigravityRequestFilterBlock(t *testing.T) {
	acc := &config.Account{AntigravityCodingFilter: "block"}
	_, err := kiroPayloadToAntigravityRequest(filterPayload(), acc, "proj-1")
	if err == nil {
		t.Fatal("expected block error, got nil")
	}
}

func TestAntigravityRequestFilterOff(t *testing.T) {
	acc := &config.Account{} // unset = off
	body, err := kiroPayloadToAntigravityRequest(filterPayload(), acc, "proj-1")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	got := systemText(t, body)
	want := "You are Claude Code, Anthropic's official CLI."
	if got != want {
		t.Errorf("systemInstruction = %q, want %q (unchanged)", got, want)
	}
}
