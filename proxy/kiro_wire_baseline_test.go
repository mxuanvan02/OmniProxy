package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the exact Kiro wire body the translators produce, so the
// refactor that moves sanitize/truncate out of them (phase 02 of
// plans/20260915-1655-kiro-history-sanitize-relocation) can be proved
// byte-identical on every fixture whose behaviour must not change.
//
// The baseline in testdata/ was captured BEFORE that refactor, from
// ClaudeToKiro alone — at that time the translator was the whole Kiro pipeline.
// After the refactor the pipeline is prepareKiroPayload(ClaudeToKiro(req)), and
// the same fixtures must hash to the same values. Regenerating the baseline
// would defeat its purpose, so -update-wire-baseline must only be used to
// re-pin behaviour after a change has been reviewed and accepted.
var updateWireBaseline = flag.Bool("update-wire-baseline", false,
	"rewrite testdata/kiro_wire_baseline.json from the current pipeline")

const wireBaselinePath = "testdata/kiro_wire_baseline.json"

type wireFixture struct {
	name string
	// request is the client payload, expressed as the JSON shape the real
	// handlers decode. Building ClaudeRequest directly does not work:
	// extractClaudeUserContent only recognises the []interface{} /
	// map[string]interface{} shape that json.Unmarshal produces.
	request map[string]interface{}
}

type wireBaseline struct {
	// SHA256 of the marshalled KiroPayload.
	Hash string `json:"hash"`
	// Body size in bytes, so a size-only drift is visible without a diff.
	Size int `json:"size"`
	// Compact structural digest: one "kind:len" token per history entry plus the
	// current message, so a mismatch shows WHERE the shapes diverged rather than
	// only that they did.
	Digest string `json:"digest"`
	// Note records why this fixture's bytes intentionally differ from the
	// pre-refactor capture. Empty means they must not differ.
	Note string `json:"note,omitempty"`
}

// kiroWireFixtures covers the shapes whose wire bytes must not move. The
// truncating fixtures are the dangerous ones: they are the only payloads where
// sanitize and truncate interact, and the ones a reordering would corrupt.
func kiroWireFixtures() []wireFixture {
	simple := []map[string]interface{}{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi there"},
		{"role": "user", "content": "what is 2+2"},
	}

	activeTool := []map[string]interface{}{
		{"role": "user", "content": "run it"},
		{"role": "assistant", "content": []map[string]interface{}{
			{"type": "tool_use", "id": "t1", "name": "run_command", "input": map[string]interface{}{"cmd": "ls"}},
		}},
		{"role": "user", "content": []map[string]interface{}{
			{"type": "tool_result", "tool_use_id": "t1", "content": "OUT_0"},
		}},
	}

	assistantFirst := []map[string]interface{}{
		{"role": "assistant", "content": []map[string]interface{}{
			{"type": "tool_use", "id": "t1", "name": "run_command", "input": map[string]interface{}{"cmd": "a"}},
		}},
		{"role": "user", "content": []map[string]interface{}{
			{"type": "tool_result", "tool_use_id": "t1", "content": "OUT_0"},
		}},
		{"role": "user", "content": "now do the next thing"},
	}

	return []wireFixture{
		{name: "plain-history-no-system", request: claudeFixture("", simple, nil)},
		{name: "plain-history-with-system", request: claudeFixture("You are a helpful agent.", simple, nil)},
		{name: "active-tool-turn-no-system", request: claudeFixture("", activeTool, toolSpecs())},
		{name: "active-tool-turn-with-system", request: claudeFixture("You are a helpful agent.", activeTool, toolSpecs())},
		{name: "assistant-first-orphan", request: claudeFixture("", assistantFirst, toolSpecs())},
		{name: "assistant-first-orphan-with-system", request: claudeFixture("You are a helpful agent.", assistantFirst, toolSpecs())},
		{name: "tool-cycles-30", request: claudeFixture("", toolCycles(30), toolSpecs())},
		{name: "tool-cycles-30-with-system", request: claudeFixture("You are a helpful agent.", toolCycles(30), toolSpecs())},
		{name: "duplicate-retry-loop", request: claudeFixture("", duplicateLoop(40), toolSpecs())},
		{name: "oversized-forces-truncate", request: claudeFixture("", bulkyHistory(40, 30_000), toolSpecs())},
		{name: "oversized-forces-truncate-with-system", request: claudeFixture("You are a helpful agent.", bulkyHistory(40, 30_000), toolSpecs())},
		{name: "oversized-with-system-and-tools", request: claudeFixture("You are a helpful agent.", bulkyToolHistory(30, 30_000), toolSpecs())},
	}
}

func claudeFixture(system string, messages []map[string]interface{}, tools []map[string]interface{}) map[string]interface{} {
	req := map[string]interface{}{
		"model":      "claude-sonnet-5",
		"max_tokens": 4096,
		"messages":   messages,
	}
	if system != "" {
		req["system"] = system
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	return req
}

func toolSpecs() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":         "run_command",
			"description":  "Run a shell command",
			"input_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"cmd": map[string]interface{}{"type": "string"}}},
		},
	}
}

// toolCycles builds n completed tool call/result rounds followed by a final
// active round, which is the shape that exercises history narration hardest.
func toolCycles(n int) []map[string]interface{} {
	out := []map[string]interface{}{{"role": "user", "content": "start"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%d", i)
		out = append(out,
			map[string]interface{}{"role": "assistant", "content": []map[string]interface{}{
				{"type": "tool_use", "id": id, "name": "run_command", "input": map[string]interface{}{"cmd": fmt.Sprintf("cmd-%d", i)}},
			}},
			map[string]interface{}{"role": "user", "content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": id, "content": fmt.Sprintf("OUT_%d", i)},
			}},
		)
	}
	out = append(out, map[string]interface{}{"role": "user", "content": "keep going"})
	return out
}

// duplicateLoop models a client stuck retrying the same failing tool, the shape
// sanitizeKiroHistory's duplicate-collapsing pass exists for.
func duplicateLoop(n int) []map[string]interface{} {
	out := []map[string]interface{}{{"role": "user", "content": "start"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%d", i)
		out = append(out,
			map[string]interface{}{"role": "assistant", "content": []map[string]interface{}{
				{"type": "tool_use", "id": id, "name": "run_command", "input": map[string]interface{}{"cmd": "same"}},
			}},
			map[string]interface{}{"role": "user", "content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": id, "content": "same error"},
			}},
		)
	}
	out = append(out, map[string]interface{}{"role": "user", "content": "stop"})
	return out
}

// bulkyHistory builds plain history large enough to cross maxPayloadBytes.
func bulkyHistory(turns, size int) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, turns*2)
	for i := 0; i < turns; i++ {
		out = append(out,
			map[string]interface{}{"role": "user", "content": bulk("U", i, size)},
			map[string]interface{}{"role": "assistant", "content": bulk("A", i, size)},
		)
	}
	out = append(out, map[string]interface{}{"role": "user", "content": "final question"})
	return out
}

// bulkyToolHistory crosses maxPayloadBytes with structured tool turns in play,
// which is the only shape where sanitize shrinking the history changes whether
// truncate fires at all.
func bulkyToolHistory(rounds, size int) []map[string]interface{} {
	out := []map[string]interface{}{{"role": "user", "content": bulk("S", 0, size)}}
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("t%d", i)
		out = append(out,
			map[string]interface{}{"role": "assistant", "content": []map[string]interface{}{
				{"type": "tool_use", "id": id, "name": "run_command", "input": map[string]interface{}{"cmd": fmt.Sprintf("cmd-%d", i)}},
			}},
			map[string]interface{}{"role": "user", "content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": id, "content": bulk("O", i, size)},
			}},
		)
	}
	out = append(out, map[string]interface{}{"role": "user", "content": "final question"})
	return out
}

func bulk(prefix string, i, size int) string {
	return fmt.Sprintf("%s%d-%s", prefix, i, strings.Repeat("x", size))
}

// kiroWireBody runs a fixture through the Kiro pipeline under test and returns
// the bytes that would be marshalled onto the wire.
//
// The baseline in testdata/ was captured before sanitize and truncate moved out
// of the translators, when ClaudeToKiro alone was the whole Kiro pipeline. The
// pipeline below is now the post-refactor equivalent, so a hash match is the
// proof that the move preserved the wire body.
func kiroWireBody(t *testing.T, fx wireFixture) []byte {
	t.Helper()

	raw, err := json.Marshal(fx.request)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	payload := prepareKiroPayload(ClaudeToKiro(&req, false))

	// AgentContinuationId is a fresh uuid per translation, so it is the one field
	// that cannot match across two runs. Blanking it does not weaken the
	// comparison: everything the refactor could plausibly disturb is history,
	// current-message content and the tool context, all of which stay in.
	payload.ConversationState.AgentContinuationId = ""

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// wireDigest summarises a body's structure so a hash mismatch points at the
// entry that changed instead of only reporting that something did.
func wireDigest(body []byte) string {
	var payload KiroPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return "<unparseable>"
	}

	parts := make([]string, 0, len(payload.ConversationState.History)+1)
	for _, msg := range payload.ConversationState.History {
		switch {
		case msg.AssistantResponseMessage != nil:
			parts = append(parts, fmt.Sprintf("a(content=%d,uses=%d)",
				len(msg.AssistantResponseMessage.Content), len(msg.AssistantResponseMessage.ToolUses)))
		case msg.UserInputMessage != nil:
			ctx := msg.UserInputMessage.UserInputMessageContext
			results, tools := 0, 0
			if ctx != nil {
				results, tools = len(ctx.ToolResults), len(ctx.Tools)
			}
			parts = append(parts, fmt.Sprintf("u(content=%d,imgs=%d,results=%d,tools=%d)",
				len(msg.UserInputMessage.Content), len(msg.UserInputMessage.Images), results, tools))
		default:
			parts = append(parts, "empty")
		}
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	curResults, curTools := 0, 0
	if cur.UserInputMessageContext != nil {
		curResults = len(cur.UserInputMessageContext.ToolResults)
		curTools = len(cur.UserInputMessageContext.Tools)
	}
	parts = append(parts, fmt.Sprintf("current(content=%d,imgs=%d,results=%d,tools=%d)",
		len(cur.Content), len(cur.Images), curResults, curTools))

	return strings.Join(parts, " ")
}

func TestKiroWireBaseline(t *testing.T) {
	fixtures := kiroWireFixtures()

	if *updateWireBaseline {
		baselines := make(map[string]wireBaseline, len(fixtures))
		for _, fx := range fixtures {
			body := kiroWireBody(t, fx)
			sum := sha256.Sum256(body)
			baselines[fx.name] = wireBaseline{
				Hash:   hex.EncodeToString(sum[:]),
				Size:   len(body),
				Digest: wireDigest(body),
			}
			t.Logf("captured %-42s size=%7d %s", fx.name, len(body), baselines[fx.name].Hash[:12])
		}
		if err := os.MkdirAll(filepath.Dir(wireBaselinePath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		dump, err := json.MarshalIndent(baselines, "", "  ")
		if err != nil {
			t.Fatalf("marshal baselines: %v", err)
		}
		if err := os.WriteFile(wireBaselinePath, append(dump, '\n'), 0o644); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
		return
	}

	raw, err := os.ReadFile(wireBaselinePath)
	if err != nil {
		t.Fatalf("read baseline (run with -update-wire-baseline to create it): %v", err)
	}
	var baselines map[string]wireBaseline
	if err := json.Unmarshal(raw, &baselines); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			want, ok := baselines[fx.name]
			if !ok {
				t.Fatalf("fixture %q is not in the baseline", fx.name)
			}

			body := kiroWireBody(t, fx)
			sum := sha256.Sum256(body)
			got := wireBaseline{
				Hash:   hex.EncodeToString(sum[:]),
				Size:   len(body),
				Digest: wireDigest(body),
			}

			if got.Hash == want.Hash {
				return
			}
			t.Errorf("wire body changed\n  want %s (size %d)\n  got  %s (size %d)\n  want digest: %s\n  got  digest: %s",
				want.Hash[:16], want.Size, got.Hash[:16], got.Size, want.Digest, got.Digest)
		})
	}
}
