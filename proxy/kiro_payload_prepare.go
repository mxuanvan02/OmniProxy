package proxy

// Kiro accepts only a single active tool turn: the last history assistant
// message may carry structured toolUses, and every earlier tool call and result
// in history must be flattened to text. That is a constraint of the Kiro API,
// not of the client request, so it cannot be applied by the translators — they
// run before the pool has picked an account, and an external account serving
// the same request accepts the full structured history and loses information
// when it is flattened. Measured on a three-round tool conversation, the
// external wire body carried 1 assistant tool_calls and 1 role:tool message
// instead of 4 and 4 (plans/20260915-1636-external-dialect-standardization/
// reports/history-tool-loss.md).
//
// The shaping therefore happens here, at dispatch, once per attempt, and on a
// copy: the caller reuses one payload pointer across the account rotation loop,
// so shaping it in place would leave a later external attempt in the same
// request holding a payload flattened for Kiro.
//
// Sanitize and truncate must stay together and in this order. Sanitize shrinks
// history by roughly three orders of magnitude on a tool-heavy fixture, so
// truncating first measures a size the wire body never reaches, fires where it
// never fired before, and changes payloadCacheKey enough to pin a different
// account.

// prepareKiroPayload returns a copy of payload shaped for the Kiro API:
// history tool activity flattened to at most one active turn, then trimmed to
// the size budget. The original is left untouched.
func prepareKiroPayload(payload *KiroPayload) *KiroPayload {
	if payload == nil {
		return nil
	}

	// Shallow copy. ConversationState, CurrentMessage and UserInputMessage are
	// value fields, so the copy owns them outright; the slices and the
	// UserInputMessageContext pointer inside them are still shared, which is why
	// history is cloned below and why truncateCurrentMessage — which writes only
	// to Content — is the only current-message mutation in this pipeline.
	cp := *payload
	cp.ConversationState.History = cloneHistoryForSanitize(payload.ConversationState.History)

	// Recomputed from the payload rather than carried over from the translator:
	// the copy is what the upstream sees, so the active-turn decision has to be
	// made against the copy's own current message.
	currentToolResultIDs := collectToolResultIDs(currentToolResultsOf(&cp))
	if currentToolResultsMatchLastAssistant(cp.ConversationState.History, currentToolResultIDs) {
		cp.ConversationState.History = sanitizeKiroHistory(cp.ConversationState.History, currentToolResultIDs)
	} else {
		cp.ConversationState.History = sanitizeKiroHistory(cp.ConversationState.History, nil)
	}

	truncatePayloadToLimit(&cp, cp.hasPriming)

	return &cp
}

// currentToolResultsOf reads the tool results carried by the outgoing message.
func currentToolResultsOf(payload *KiroPayload) []KiroToolResult {
	if payload == nil {
		return nil
	}
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil {
		return nil
	}
	return ctx.ToolResults
}

// cloneHistoryForSanitize copies the history entries that sanitizeKiroHistory
// mutates, so shaping the copy cannot reach the caller's payload.
//
// sanitize writes through the message pointers — it reassigns Content, nils
// ToolUses, and nils UserInputMessageContext — so copying the slice of structs
// is not enough: every pointer it dereferences needs its own struct. That is
// the two levels below.
//
// The slices inside those structs are deliberately shared. sanitize only nils
// those fields, never edits their elements, and rewriting a 30-round tool
// history per attempt to protect data nothing writes would cost more than it
// protects. A JSON round-trip is not an option here for the same reason it is
// attractive: KiroToolUse.Input is a map[string]interface{}, so re-encoding
// would reformat numbers and change the bytes sent upstream.
func cloneHistoryForSanitize(history []KiroHistoryMessage) []KiroHistoryMessage {
	if len(history) == 0 {
		return history
	}

	out := make([]KiroHistoryMessage, len(history))
	for i, msg := range history {
		if u := msg.UserInputMessage; u != nil {
			userCopy := *u
			// The context is a pointer sanitize writes through, so it needs its
			// own struct too. Without this, nilling ToolResults on the copy would
			// nil it on the caller's payload as well.
			if ctx := u.UserInputMessageContext; ctx != nil {
				ctxCopy := *ctx
				userCopy.UserInputMessageContext = &ctxCopy
			}
			out[i].UserInputMessage = &userCopy
		}
		if a := msg.AssistantResponseMessage; a != nil {
			assistantCopy := *a
			out[i].AssistantResponseMessage = &assistantCopy
		}
	}
	return out
}
