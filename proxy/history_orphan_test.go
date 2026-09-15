package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Helpers for building Kiro history directly, so the pairing invariant can be
// exercised without going through a translator.

func asstWithTool(ids ...string) KiroHistoryMessage {
	uses := make([]KiroToolUse, 0, len(ids))
	for _, id := range ids {
		uses = append(uses, KiroToolUse{ToolUseID: id, Name: "run_command"})
	}
	return KiroHistoryMessage{
		AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: uses},
	}
}

func userWithResults(ids ...string) KiroHistoryMessage {
	results := make([]KiroToolResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, KiroToolResult{
			ToolUseID: id,
			Content:   []KiroResultContent{{Text: "out-" + id}},
		})
	}
	return KiroHistoryMessage{
		UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &UserInputMessageContext{ToolResults: results},
		},
	}
}

func userWithTextAndResults(text string, ids ...string) KiroHistoryMessage {
	msg := userWithResults(ids...)
	msg.UserInputMessage.Content = text
	return msg
}

func userWithText(text string) KiroHistoryMessage {
	return KiroHistoryMessage{UserInputMessage: &KiroUserInputMessage{Content: text}}
}

// orphanedResultIDs reports every tool result in history whose tool call does not
// appear earlier in the same history. This is the invariant the fix establishes,
// and the only reason phase 02 can remove the narration that used to hide it.
func orphanedResultIDs(history []KiroHistoryMessage) []string {
	live := make(map[string]bool)
	var orphans []string
	for i := range history {
		msg := history[i]
		if a := msg.AssistantResponseMessage; a != nil {
			for _, u := range a.ToolUses {
				live[u.ToolUseID] = true
			}
			continue
		}
		if u := msg.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			for _, r := range u.UserInputMessageContext.ToolResults {
				if !live[r.ToolUseID] {
					orphans = append(orphans, r.ToolUseID)
				}
			}
		}
	}
	return orphans
}

// allResultIDs flattens the surviving tool results across the whole history, so
// assertions do not depend on which index happens to hold the user turn.
func allResultIDs(history []KiroHistoryMessage) []string {
	var ids []string
	for _, msg := range history {
		if u := msg.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			for _, r := range u.UserInputMessageContext.ToolResults {
				ids = append(ids, r.ToolUseID)
			}
		}
	}
	return ids
}

func allTexts(history []KiroHistoryMessage) []string {
	var texts []string
	for _, msg := range history {
		if u := msg.UserInputMessage; u != nil && u.Content != "" {
			texts = append(texts, u.Content)
		}
	}
	return texts
}

func TestStripOrphanedToolResults(t *testing.T) {
	cases := []struct {
		name      string
		history   []KiroHistoryMessage
		wantIDs   []string
		wantKeeps string // text that must survive the pass, if any
	}{
		{
			name:    "leading assistant trimmed, result left behind",
			history: []KiroHistoryMessage{userWithResults("t1"), userWithText("next")},
		},
		{
			name:    "paired result is untouched",
			history: []KiroHistoryMessage{asstWithTool("t1"), userWithResults("t1"), userWithText("next")},
			wantIDs: []string{"t1"},
		},
		{
			name:    "assistant with no tool uses pairs nothing",
			history: []KiroHistoryMessage{userWithResults("t1")},
		},
		{
			name:      "text survives, only the orphan result is stripped",
			history:   []KiroHistoryMessage{userWithTextAndResults("real question", "t1")},
			wantKeeps: "real question",
		},
		{
			name:    "call appearing later does not pair an earlier result",
			history: []KiroHistoryMessage{userWithResults("t1"), asstWithTool("t1")},
		},
		{
			name:    "partially answered turn keeps only the paired result",
			history: []KiroHistoryMessage{asstWithTool("t1"), userWithResults("t1", "t2")},
			wantIDs: []string{"t1"},
		},
		{
			name:    "history without orphans is returned unchanged",
			history: []KiroHistoryMessage{userWithText("hello"), asstWithTool("t1"), userWithResults("t1")},
			wantIDs: []string{"t1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripOrphanedToolResults(tc.history)

			if orphans := orphanedResultIDs(got); len(orphans) != 0 {
				t.Fatalf("invariant violated: orphaned results %v remain", orphans)
			}
			if ids := allResultIDs(got); !equalStrings(ids, tc.wantIDs) {
				t.Errorf("surviving results = %v, want %v", ids, tc.wantIDs)
			}
			if tc.wantKeeps != "" {
				found := false
				for _, text := range allTexts(got) {
					if text == tc.wantKeeps {
						found = true
					}
				}
				if !found {
					t.Errorf("text %q did not survive; got %v", tc.wantKeeps, allTexts(got))
				}
			}
		})
	}
}

// The pass rebuilds the result slice rather than compacting it in place, so a
// caller that still holds the original slice sees it untouched. That matters
// once the payload is a copy sharing slices with the request-scoped original.
//
// Note the limit: the pass reassigns fields *through* the message pointers, so a
// shallow copy of the slice does not isolate those structs. Only the result
// slice's backing array is guaranteed untouched.
func TestStripOrphanedToolResultsRewritesSliceNotArray(t *testing.T) {
	history := []KiroHistoryMessage{userWithResults("t1", "t2")}
	original := history[0].UserInputMessage.UserInputMessageContext.ToolResults
	snapshot := append([]KiroToolResult(nil), original...)

	stripOrphanedToolResults(history)

	for i := range snapshot {
		if !reflect.DeepEqual(original[i], snapshot[i]) {
			t.Fatalf("backing array element %d was overwritten: %+v -> %+v",
				i, snapshot[i], original[i])
		}
	}
}

// Phase 02 removes the narration that currently hides an orphaned result from
// every upstream. Until then this passes trivially, which is the point: it is
// the guard that must keep holding once the narration is gone.
func TestClaudeToKiroLeavesNoOrphanedToolResults(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"model": "claude-sonnet-5", "max_tokens": 1024,
		"messages": []map[string]interface{}{
			{"role": "assistant", "content": []map[string]interface{}{
				{"type": "tool_use", "id": "t1", "name": "run_command", "input": map[string]interface{}{"cmd": "a"}},
			}},
			{"role": "user", "content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": "t1", "content": "OUT"},
			}},
			{"role": "user", "content": "now do the next thing"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	payload := ClaudeToKiro(&req, false)
	if orphans := orphanedResultIDs(payload.ConversationState.History); len(orphans) != 0 {
		t.Fatalf("orphaned results %v reached the payload", orphans)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
