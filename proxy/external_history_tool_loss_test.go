package proxy

import (
	"encoding/json"
	"testing"

	"omniproxy/config"
)

// TestExternalProviderReceivesFullToolHistory is the acceptance test for the
// refactor that moved Kiro's history shaping out of the translators. An external
// account accepts the full structured tool history, so every tool call the
// client sent must reach it; the same payload aimed at Kiro must still be
// flattened, because that constraint has not gone away.
//
// Measured before the refactor: the external body carried 1 assistant tool_calls
// and 1 role:tool message for a 4-cycle conversation. The diagnosis was that the
// translators applied Kiro's constraint before the pool had picked an account
// (plans/20260915-1636-external-dialect-standardization/reports/tool-loss*).
//
// The request is built by unmarshalling JSON, exactly as the HTTP handler does:
// ClaudeRequest.Content is interface{}, and extractClaudeUserContent only
// recognises the []interface{} shape that json.Unmarshal produces. Constructing
// the struct directly with []ClaudeContentBlock silently yields an empty
// conversation, which looks like a far worse bug than the real one.
func TestExternalProviderReceivesFullToolHistory(t *testing.T) {
	const cycles = 3
	const activeID = "toolu_active"
	type block = map[string]interface{}
	userBlocks := func(bs ...block) []block { return bs }

	msgs := []block{{"role": "user", "content": "start the task"}}
	for i := 0; i < cycles; i++ {
		id := "toolu_hist_" + string(rune('a'+i))
		// Distinct outputs per cycle: sanitizeKiroHistory collapses consecutive
		// *identical* narrated tool-result turns, so reusing one output would
		// understate how much the Kiro path drops.
		msgs = append(msgs,
			block{"role": "assistant", "content": []block{
				{"type": "tool_use", "id": id, "name": "run_command", "input": block{"cmd": "step " + id}},
			}},
			block{"role": "user", "content": userBlocks(
				block{"type": "tool_result", "tool_use_id": id, "content": "OUTPUT_" + string(rune('a'+i))},
			)},
		)
	}
	// Active turn: the assistant tool call the current message answers.
	msgs = append(msgs,
		block{"role": "assistant", "content": []block{
			{"type": "tool_use", "id": activeID, "name": "run_command", "input": block{"cmd": "final"}},
		}},
		block{"role": "user", "content": userBlocks(
			block{"type": "tool_result", "tool_use_id": activeID, "content": "OUTPUT_LAST"},
			block{"type": "text", "text": "now do the next thing"},
		)},
	)

	raw, err := json.Marshal(block{
		"model": "claude-sonnet-5", "max_tokens": 1024,
		"messages": msgs,
		"tools": []block{{
			"name": "run_command", "description": "run",
			"input_schema": block{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	// The client sent cycles completed tool calls plus one active turn.
	const wantCalls = cycles + 1

	account := &config.Account{
		ID:          "ext-toolhistory",
		AuthMethod:  "external_openai",
		BaseURL:     "https://example.invalid",
		AccessToken: "sk-x",
	}

	extBody, err := kiroPayloadToOpenAIRequest(ClaudeToKiro(&req, false), account)
	external := countWireToolActivity(t, extBody, err)
	t.Logf("external dialect: assistant turns carrying tool_calls=%d, role:tool messages=%d (client sent %d tool calls)",
		external.assistantWithCalls, external.toolMsgs, wantCalls)

	if external.assistantWithCalls != wantCalls {
		t.Errorf("external body carries %d assistant tool_calls, want %d — the client's tool history was dropped before it reached the provider",
			external.assistantWithCalls, wantCalls)
	}
	if external.toolMsgs != wantCalls {
		t.Errorf("external body carries %d role:tool messages, want %d", external.toolMsgs, wantCalls)
	}

	// The Kiro branch must still flatten, or the constraint was simply deleted
	// rather than moved.
	kiroBody, err := kiroPayloadToOpenAIRequest(prepareKiroPayload(ClaudeToKiro(&req, false)), account)
	kiro := countWireToolActivity(t, kiroBody, err)
	t.Logf("kiro-shaped payload: assistant turns carrying tool_calls=%d, role:tool messages=%d",
		kiro.assistantWithCalls, kiro.toolMsgs)

	if kiro.assistantWithCalls != 1 || kiro.toolMsgs != 1 {
		t.Errorf("kiro-shaped body carries %d assistant tool_calls and %d role:tool messages, want exactly 1 and 1 (only the active turn stays structured)",
			kiro.assistantWithCalls, kiro.toolMsgs)
	}
}

// wireToolActivity counts the structured tool turns in a built external body.
type wireToolActivity struct {
	assistantTurns     int
	assistantWithCalls int
	toolMsgs           int
}

func countWireToolActivity(t *testing.T, body map[string]interface{}, err error) wireToolActivity {
	t.Helper()
	if err != nil {
		t.Fatalf("build external body: %v", err)
	}

	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal built body: %v", err)
	}
	var out struct {
		Messages []struct {
			Role  string `json:"role"`
			Calls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(wire, &out); err != nil {
		t.Fatalf("unmarshal built body: %v", err)
	}

	var got wireToolActivity
	for _, m := range out.Messages {
		switch m.Role {
		case "assistant":
			got.assistantTurns++
			if len(m.Calls) > 0 {
				got.assistantWithCalls++
			}
		case "tool":
			got.toolMsgs++
		}
	}
	return got
}
