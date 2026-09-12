// Package proxy: content_block_retry.go
//
// The recovery loop that turns a content-scanner rejection from a lost turn
// into a served one, and turns each recovery into knowledge that makes the
// next request cheaper.
//
// See content_block_evasion.go for the experimental basis (the scanner compares
// raw bytes and does not normalise Unicode) and for the risk note: this works
// around a provider's own content control and is opt-in per account.
//
// # SHAPE OF THE LOOP
//
// On a content-blocked verdict, for an account with ContentBlockEvasion on:
//
//	attempt 1..N  obfuscate a shrinking set of candidate terms, retry
//	on success    record the terms that worked, so later requests pre-empt them
//	on exhaustion give up and let the caller fail over as before
//
// The candidate set is taken from the request's own text, ranked by how much a
// keyword list would plausibly care about each token. It is NOT a static
// blocklist — that was tried and reverted, because the terms that actually
// trigger this provider are not the ones a human would guess.
//
// # WHY THE ATTEMPT BUDGET IS SMALL
//
// Every attempt is a real request against the operator's credential, and this
// provider throttles a burst by lowering its own block threshold — probing it
// hard makes everything look blocked and poisons the result. Four attempts is
// enough to separate "one distinctive term" from "cannot be recovered", and
// small enough not to trip that behaviour.
package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"omniproxy/config"
	"omniproxy/logger"
	"omniproxy/pool"
)

// contentBlockEvasionEnabled reports whether this account opted in.
//
// Deliberately requires an explicit per-account flag: the technique is evasion
// of a provider's content control, and only the operator can accept that risk
// for a given credential.
func contentBlockEvasionEnabled(account *config.Account) bool {
	return account != nil && account.ContentBlockEvasion
}

// cloneKiroPayload returns an independent retry payload. JSON round-tripping
// deep-copies every field sent upstream; fields excluded from JSON are restored
// explicitly because adapters still need them for routing and translation.
func cloneKiroPayload(payload *KiroPayload) (*KiroPayload, error) {
	if payload == nil {
		return nil, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	clone := &KiroPayload{}
	if err := json.Unmarshal(raw, clone); err != nil {
		return nil, err
	}
	clone.ToolChoice = cloneSchemaValue(payload.ToolChoice)
	clone.ToolNameMap = make(map[string]string, len(payload.ToolNameMap))
	for key, value := range payload.ToolNameMap {
		clone.ToolNameMap[key] = value
	}
	clone.OriginalModel = payload.OriginalModel
	clone.PublicModel = payload.PublicModel
	if payload.InferenceConfig != nil && clone.InferenceConfig != nil {
		clone.InferenceConfig.ReasoningEffort = payload.InferenceConfig.ReasoningEffort
		if payload.InferenceConfig.Thinking != nil {
			thinking := *payload.InferenceConfig.Thinking
			clone.InferenceConfig.Thinking = &thinking
		}
	}
	return clone, nil
}

// tryContentBlockRecovery re-sends a rejected payload with candidate terms
// split by a zero-width character, and reports whether one of those attempts
// was served.
//
// Contract with the caller: this is only called after the caller has already
// established that err is a content-blocked verdict. On true the response has
// been streamed to the client and the caller must treat the account as
// successful; on false nothing was emitted and the caller proceeds with its
// normal failover.
//
// The payload is mutated in place on the attempt that succeeds — the caller has
// no further use for the original bytes, and copying a 400KB payload per
// attempt to preserve them would cost more than it protects.
func (h *Handler) tryContentBlockRecovery(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	callback *KiroStreamCallback,
	model string,
	err error,
) bool {
	if !contentBlockEvasionEnabled(account) || payload == nil || err == nil {
		return false
	}
	if !pool.IsContentBlockedError(err) {
		return false
	}
	// A client that has already hung up must not be billed for retries.
	if clientGone(ctx, err) {
		return false
	}

	// Capture first: if every attempt below fails, this file is the only
	// evidence left of what the provider rejected. Written before the retries
	// so it survives even if the process dies mid-recovery.
	captureBlockedPayload(config.DataDir(), account.Email, model, payload, err)

	// Learned terms are applied and tried as one attempt, because a term proven
	// on an earlier request is far likelier than anything the ranking guesses.
	// applyLearnedObfuscation is also called on the happy path before the first
	// upstream attempt, so reaching here means either nothing is learned yet or
	// the learned set was not sufficient.
	candidates := extractCandidateTerms(payload, 12)
	if len(candidates) == 0 {
		logger.Warnf("[ContentBlock] %s: no candidate terms in payload — cannot recover, failing over", account.Email)
		return false
	}

	// Attempt schedule, widest net first. Recovering on attempt 1 is the common
	// case and the cheapest; the narrowing steps exist to identify WHICH term
	// mattered, which is the part worth remembering.
	//
	//	1: all candidates          — most likely to pass, tells us least
	//	2: top half                 — halves the suspect set
	//	3: top quarter
	//	4: single most distinctive  — pins one term exactly
	schedule := bisectSchedule(candidates, maxBisectAttempts)

	for i, terms := range schedule {
		attempt, cloneErr := cloneKiroPayload(payload)
		if cloneErr != nil {
			logger.Warnf("[ContentBlock] %s: cannot clone payload for recovery: %v", account.Email, cloneErr)
			return false
		}
		if !obfuscateCandidates(attempt, terms) {
			continue
		}
		if callback != nil && callback.OnReset != nil {
			callback.OnReset()
		}
		logger.Warnf("[ContentBlock] %s: recovery attempt %d/%d obfuscating %d term(s) (%s)",
			account.Email, i+1, len(schedule), len(terms), summariseTerms(terms))

		retryErr := dispatchChat(ctx, account, attempt, callback)
		if retryErr == nil {
			// Only terms from a *successful* attempt are learned. This is the
			// whole reason the loop narrows instead of stopping at attempt 1:
			// a win with 12 terms obfuscated teaches nothing reusable, a win
			// with 1 identifies the trigger.
			learnedTerms.learn(terms)
			logger.Infof("[ContentBlock] %s: recovered on attempt %d by obfuscating %d term(s)",
				account.Email, i+1, len(terms))
			*payload = *attempt
			return true
		}
		if !pool.IsContentBlockedError(retryErr) {
			// The verdict changed — this is no longer the problem we are
			// solving (quota, auth, transport). Hand it back rather than
			// spending the remaining attempts on the wrong failure.
			logger.Warnf("[ContentBlock] %s: verdict changed on attempt %d, stopping recovery: %s",
				account.Email, i+1, truncateForLog(retryErr.Error()))
			return false
		}
		if clientGone(ctx, retryErr) {
			return false
		}
		// A retry that emitted partial output cannot be replayed without
		// duplicating the prefix the client already saw.
		if callback != nil && callback.HasOutput != nil && callback.HasOutput() {
			logger.Warnf("[ContentBlock] %s: attempt %d produced partial output; stopping recovery", account.Email, i+1)
			return false
		}
	}

	// Last resort: split every candidate-shaped token in the payload. This is
	// the most likely to pass and the most expensive — each splitter adds
	// tokens — so it runs only after the targeted attempts failed, and teaches
	// nothing (a win here identifies no specific term).
	attempt, cloneErr := cloneKiroPayload(payload)
	if cloneErr != nil {
		logger.Warnf("[ContentBlock] %s: cannot clone payload for last-resort recovery: %v", account.Email, cloneErr)
		return false
	}
	if obfuscateEverything(attempt) {
		if callback != nil && callback.OnReset != nil {
			callback.OnReset()
		}
		logger.Warnf("[ContentBlock] %s: recovery last resort — obfuscating all candidate tokens", account.Email)
		if retryErr := dispatchChat(ctx, account, attempt, callback); retryErr == nil {
			logger.Infof("[ContentBlock] %s: recovered with full-payload obfuscation (no single term identified)", account.Email)
			*payload = *attempt
			return true
		}
	}

	logger.Warnf("[ContentBlock] %s: recovery exhausted for model %s — failing over", account.Email, model)
	return false
}

// bisectSchedule builds the shrinking candidate sets to try, widest first.
//
// Returned sets are prefixes of the ranked candidate list, so the single-term
// attempt is always the highest-ranked token — the one most likely to be on a
// keyword list.
func bisectSchedule(candidates []string, maxAttempts int) [][]string {
	if len(candidates) == 0 || maxAttempts <= 0 {
		return nil
	}
	seen := map[int]bool{}
	out := make([][]string, 0, maxAttempts)
	n := len(candidates)
	for len(out) < maxAttempts && n >= 1 {
		if !seen[n] {
			seen[n] = true
			out = append(out, candidates[:n])
		}
		if n == 1 {
			break
		}
		n /= 2
	}
	// Guarantee the single-term probe is present: without it a recovery can
	// succeed without ever identifying a reusable term.
	if !seen[1] && len(out) > 0 {
		out = append(out, candidates[:1])
	}
	return out
}

// summariseTerms renders a short, log-safe preview of the obfuscation set.
func summariseTerms(terms []string) string {
	const maxShown = 4
	if len(terms) <= maxShown {
		return strings.Join(terms, ", ")
	}
	return strings.Join(terms[:maxShown], ", ") + ", …"
}
