package proxy

import (
	"path/filepath"
	"strings"
	"testing"

	"omniproxy/config"
)

// The scanner comparison established experimentally: a flagged token passes
// once a zero-width character splits it, and the visible text is unchanged.
func TestObfuscateTokenSplitsButPreservesVisibleText(t *testing.T) {
	got := obfuscateToken("GODMODE")
	if got == "GODMODE" {
		t.Fatal("token was not split")
	}
	if !strings.Contains(got, zeroWidthSplitter) {
		t.Fatalf("expected a zero-width splitter in %q", got)
	}
	// A reader (and the tokenizer, after stripping) must still see the word.
	if stripped := strings.ReplaceAll(got, zeroWidthSplitter, ""); stripped != "GODMODE" {
		t.Fatalf("visible text changed: %q -> %q", "GODMODE", stripped)
	}
}

// Short tokens are left alone: the splitter would be a large fraction of the
// token, and tokens that short are too common to be plausible scanner targets.
func TestObfuscateTokenLeavesShortTokensAlone(t *testing.T) {
	for _, tok := range []string{"", "a", "ok", "GPT"} {
		if got := obfuscateToken(tok); got != tok {
			t.Errorf("obfuscateToken(%q) = %q, want unchanged", tok, got)
		}
	}
}

// Re-obfuscating an already-split token would add splitters on every request
// and grow the payload without bound. dispatchChat applies learned terms on
// every call, so idempotence is a correctness requirement, not a nicety.
func TestObfuscationIsIdempotent(t *testing.T) {
	once := obfuscateToken("SENSITIVE")
	twice, changed := obfuscateTermsIn(once, []string{"SENSITIVE"})
	if changed {
		t.Error("already-obfuscated text was reported as modified")
	}
	if twice != once {
		t.Errorf("second pass changed the text:\n%q\n%q", once, twice)
	}
}

// Each recovery attempt must start from the exact rejected payload. A shallow
// copy shares history slices and context pointers, so obfuscating a failed
// attempt silently mutates the original and contaminates every later attempt.
func TestCloneKiroPayloadIsolatesNestedRetryMutation(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "current SENSITIVE text",
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: []KiroToolResult{{
				Content: []KiroResultContent{{Text: "result SENSITIVE text"}},
			}},
		},
	}
	payload.ConversationState.History = []KiroHistoryMessage{{
		UserInputMessage: &KiroUserInputMessage{Content: "history SENSITIVE text"},
	}}

	attempt, err := cloneKiroPayload(payload)
	if err != nil {
		t.Fatalf("cloneKiroPayload: %v", err)
	}
	if !obfuscateCandidates(attempt, []string{"SENSITIVE"}) {
		t.Fatal("retry clone was not obfuscated")
	}

	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.Content != "current SENSITIVE text" {
		t.Errorf("original current message mutated: %q", current.Content)
	}
	if got := current.UserInputMessageContext.ToolResults[0].Content[0].Text; got != "result SENSITIVE text" {
		t.Errorf("original tool result mutated: %q", got)
	}
	if got := payload.ConversationState.History[0].UserInputMessage.Content; got != "history SENSITIVE text" {
		t.Errorf("original history mutated: %q", got)
	}
}

func TestObfuscateTermsInIsCaseInsensitive(t *testing.T) {
	out, changed := obfuscateTermsIn("the godmode flag", []string{"GODMODE"})
	if !changed {
		t.Fatal("lowercase occurrence was not matched")
	}
	if strings.Contains(out, "godmode") {
		t.Errorf("term left intact: %q", out)
	}
	if stripped := strings.ReplaceAll(out, zeroWidthSplitter, ""); stripped != "the godmode flag" {
		t.Errorf("visible text changed: %q", stripped)
	}
}

// Tool names are identifiers the client matches responses against, and
// toolUseId correlates a call with its result. A zero-width character in
// either silently breaks tool calling — a far worse outcome than the
// rejection this module exists to avoid. Same for image bytes, where a
// splitter corrupts the base64.
func TestObfuscationNeverTouchesIdentifiersOrBinary(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "please run SENSITIVE_TOOL now",
		Images: []KiroImage{{
			Format: "png",
			Source: struct {
				Bytes string `json:"bytes"`
			}{Bytes: "SENSITIVEBASE64DATA"},
		}},
		UserInputMessageContext: &UserInputMessageContext{
			Tools: []KiroToolWrapper{func() KiroToolWrapper {
				var w KiroToolWrapper
				w.ToolSpecification.Name = "SENSITIVE_TOOL"
				w.ToolSpecification.Description = "runs the SENSITIVE_TOOL action"
				return w
			}()},
			ToolResults: []KiroToolResult{{
				ToolUseID: "SENSITIVE_CALL_ID",
				Content:   []KiroResultContent{{Text: "SENSITIVE output"}},
			}},
		},
	}
	payload.ConversationState.History = []KiroHistoryMessage{{
		AssistantResponseMessage: &KiroAssistantResponseMessage{
			Content: "calling SENSITIVE_TOOL",
			ToolUses: []KiroToolUse{{
				ToolUseID: "SENSITIVE_CALL_ID",
				Name:      "SENSITIVE_TOOL",
			}},
		},
	}}

	if !obfuscateCandidates(payload, []string{"SENSITIVE_TOOL", "SENSITIVE", "SENSITIVEBASE64DATA", "SENSITIVE_CALL_ID"}) {
		t.Fatal("expected some prose to be obfuscated")
	}

	msg := payload.ConversationState.CurrentMessage.UserInputMessage
	tool := msg.UserInputMessageContext.Tools[0].ToolSpecification
	if tool.Name != "SENSITIVE_TOOL" {
		t.Errorf("tool name was obfuscated: %q", tool.Name)
	}
	if msg.Images[0].Source.Bytes != "SENSITIVEBASE64DATA" {
		t.Error("image bytes were obfuscated")
	}
	if got := msg.UserInputMessageContext.ToolResults[0].ToolUseID; got != "SENSITIVE_CALL_ID" {
		t.Errorf("toolUseId was obfuscated: %q", got)
	}
	if got := payload.ConversationState.History[0].AssistantResponseMessage.ToolUses[0].Name; got != "SENSITIVE_TOOL" {
		t.Errorf("history tool name was obfuscated: %q", got)
	}
	if got := payload.ConversationState.History[0].AssistantResponseMessage.ToolUses[0].ToolUseID; got != "SENSITIVE_CALL_ID" {
		t.Errorf("history toolUseId was obfuscated: %q", got)
	}

	// Prose, by contrast, must have been rewritten — otherwise the module
	// would pass this test by doing nothing at all.
	if !strings.Contains(msg.Content, zeroWidthSplitter) {
		t.Errorf("user prose was not obfuscated: %q", msg.Content)
	}
	if !strings.Contains(tool.Description, zeroWidthSplitter) {
		t.Errorf("tool description was not obfuscated: %q", tool.Description)
	}
}

// The whole point of narrowing is to end on a single term: a win with twelve
// terms obfuscated teaches nothing reusable.
func TestBisectScheduleAlwaysEndsAtOneTerm(t *testing.T) {
	candidates := []string{"a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8", "i9", "j10", "k11", "l12"}
	schedule := bisectSchedule(candidates, maxBisectAttempts)

	if len(schedule) == 0 {
		t.Fatal("empty schedule")
	}
	if len(schedule) > maxBisectAttempts+1 {
		t.Fatalf("schedule has %d attempts, budget is %d", len(schedule), maxBisectAttempts)
	}
	if got := len(schedule[0]); got != len(candidates) {
		t.Errorf("first attempt covers %d terms, want all %d", got, len(candidates))
	}
	if got := len(schedule[len(schedule)-1]); got != 1 {
		t.Errorf("last attempt covers %d terms, want exactly 1", got)
	}
	// Each step must strictly narrow, or an attempt is wasted repeating one.
	for i := 1; i < len(schedule); i++ {
		if len(schedule[i]) >= len(schedule[i-1]) {
			t.Errorf("attempt %d (%d terms) does not narrow attempt %d (%d terms)",
				i+1, len(schedule[i]), i, len(schedule[i-1]))
		}
	}
}

func TestBisectScheduleHandlesDegenerateInput(t *testing.T) {
	if got := bisectSchedule(nil, 4); got != nil {
		t.Errorf("nil candidates -> %v, want nil", got)
	}
	if got := bisectSchedule([]string{"only"}, 0); got != nil {
		t.Errorf("zero budget -> %v, want nil", got)
	}
	single := bisectSchedule([]string{"only"}, 4)
	if len(single) != 1 || len(single[0]) != 1 {
		t.Errorf("single candidate -> %v, want one attempt of one term", single)
	}
}

// Distinctive, rare identifiers must be tried before ordinary prose: a keyword
// list is far likelier to hold SCREAMING_IDENTIFIERS than common words.
func TestCandidateRankingPrefersDistinctiveTokens(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "please review this code and then review it again, review carefully ULTRAPLINIAN",
	}
	terms := extractCandidateTerms(payload, 5)
	if len(terms) == 0 {
		t.Fatal("no candidates extracted")
	}
	if !strings.EqualFold(terms[0], "ULTRAPLINIAN") {
		t.Errorf("top candidate = %q, want ULTRAPLINIAN (rare + all-caps)", terms[0])
	}
}

// Evasion must never engage on its own: it works around a provider's content
// control, and only the operator can accept that risk per credential.
func TestEvasionIsOptInPerAccount(t *testing.T) {
	if contentBlockEvasionEnabled(nil) {
		t.Error("nil account must not be treated as opted in")
	}
	if contentBlockEvasionEnabled(&config.Account{Email: "x"}) {
		t.Error("account defaults to evasion enabled — must be opt-in")
	}
	if !contentBlockEvasionEnabled(&config.Account{ContentBlockEvasion: true}) {
		t.Error("explicit opt-in was not honoured")
	}
}

func TestLearnedTermsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	resetLearnedTermsForTests()
	t.Cleanup(resetLearnedTermsForTests)

	InitContentBlockStore(dir)
	learnedTerms.learn([]string{"TRIGGERWORD"})

	if _, err := filepath.Glob(filepath.Join(dir, "content-block-terms.json")); err != nil {
		t.Fatalf("glob: %v", err)
	}

	// Simulate a restart: drop in-memory state, reload from disk.
	resetLearnedTermsForTests()
	InitContentBlockStore(dir)

	snap := learnedTerms.snapshot()
	found := false
	for _, term := range snap {
		if strings.EqualFold(term, "TRIGGERWORD") {
			found = true
		}
	}
	if !found {
		t.Errorf("learned term did not survive restart, got %v", snap)
	}
}

// An unbounded store would slow every later request, since each term is
// scanned for on every payload.
func TestLearnedTermsAreCapped(t *testing.T) {
	resetLearnedTermsForTests()
	t.Cleanup(resetLearnedTermsForTests)

	bulk := make([]string, 0, maxLearnedTerms*2)
	for i := 0; i < maxLearnedTerms*2; i++ {
		bulk = append(bulk, "term"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}
	learnedTerms.learn(bulk)

	if got := len(learnedTerms.snapshot()); got > maxLearnedTerms {
		t.Errorf("store holds %d terms, cap is %d", got, maxLearnedTerms)
	}
}

// Learning a term is what makes the next request cheap; applying it must
// actually rewrite the payload.
func TestApplyLearnedObfuscationRewritesKnownTerms(t *testing.T) {
	resetLearnedTermsForTests()
	t.Cleanup(resetLearnedTermsForTests)

	learnedTerms.learn([]string{"BLOCKEDTERM"})

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "a prompt mentioning BLOCKEDTERM inline",
	}
	if !applyLearnedObfuscation(payload) {
		t.Fatal("learned term was not applied")
	}
	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Contains(got, "BLOCKEDTERM") {
		t.Errorf("term left intact: %q", got)
	}
	if stripped := strings.ReplaceAll(got, zeroWidthSplitter, ""); stripped != "a prompt mentioning BLOCKEDTERM inline" {
		t.Errorf("visible text changed: %q", stripped)
	}
}

func TestApplyLearnedObfuscationIsNoOpWhenNothingLearned(t *testing.T) {
	resetLearnedTermsForTests()
	t.Cleanup(resetLearnedTermsForTests)

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{Content: "plain text"}
	if applyLearnedObfuscation(payload) {
		t.Error("reported a change with an empty store")
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "plain text" {
		t.Error("payload was modified with an empty store")
	}
}
