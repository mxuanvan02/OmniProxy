// Package proxy — dialect selection and one-way learning for external accounts.
//
// An external account states which wire dialect its gateway speaks, but the
// statement is a guess made when the key was added: a reseller may serve only
// chat completions, only Responses, or both, and nothing in the key reveals
// which. This file turns a request-time 404/405 into a permanent correction
// rather than a failing turn.
package proxy

import (
	"context"
	"errors"

	"omniproxy/config"
	"omniproxy/logger"
	"omniproxy/pool"
)

// dispatchExternalDialect sends the turn over the account's configured dialect,
// replaying it on chat when the upstream proves that dialect does not exist.
//
// The replay is narrow on purpose. It fires only for a failure that names the
// route itself (see externalResponsesDialectUnsupported) and only while nothing
// has reached the client: an upstream that answered 404 after streaming half an
// answer would otherwise make the client render that answer twice. Everything
// else — 400, 401, 402, 403, 429, 5xx — returns untouched, so the pool's
// auth-failure and failover handling still see the status it classifies on.
//
// The Anthropic dialect has no equivalent: a gateway either exposes
// /v1/messages or it does not, and such a gateway is not a chat-completions
// reseller to fall back to, so guessing would trade a clear error for a wrong
// one.
func dispatchExternalDialect(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	switch externalAPIDialect(account) {
	case "responses":
		err := CallExternalOpenAIResponses(ctx, account, payload, callback)
		if err == nil || !errors.Is(err, errExternalDialectUnsupported) {
			return err
		}
		if !externalAttemptProduced(callback) {
			rememberExternalDialectChat(account)
			return CallExternalOpenAI(ctx, account, payload, callback)
		}
		// The client already has part of an answer, so replaying the turn would
		// duplicate it. The account keeps the dialect it was configured with;
		// the next request is free to learn it.
		logger.Warnf("[ExternalDialect] account=%s dialect=responses fallback skipped: upstream answered "+
			"unsupported dialect after output had been sent", accountLabel(account))
		return err
	case "anthropic":
		return CallExternalAnthropic(ctx, account, payload, callback)
	}
	return CallExternalOpenAI(ctx, account, payload, callback)
}

// externalAttemptProduced reports whether the attempt that just failed already
// put visible output in front of the client.
//
// An unwired HasOutput is read as "produced": without the signal there is no
// way to tell a failed first byte from a half-delivered answer, and duplicating
// an answer is worse than failing a turn that a retry would have fixed anyway.
func externalAttemptProduced(callback *KiroStreamCallback) bool {
	if callback == nil || callback.HasOutput == nil {
		logger.Warnf("[ExternalDialect] callback has no HasOutput probe; refusing to replay the turn")
		return true
	}
	return callback.HasOutput()
}

// rememberExternalDialectChat records that an account's gateway does not
// implement the dialect it was configured with, so the next request goes
// straight to chat instead of paying for the same 404 again.
//
// A failure to persist is logged and ignored: the call in flight is about to
// succeed on chat regardless, and losing the note costs one round trip per
// request rather than correctness.
func rememberExternalDialectChat(account *config.Account) {
	if account == nil {
		return
	}
	logger.Warnf("[ExternalDialect] account=%s dialect=responses unsupported upstream; switching to chat",
		accountLabel(account))
	account.ExternalAPIDialect = "chat"
	if err := config.SetAccountExternalAPIDialect(account.ID, "chat"); err != nil {
		logger.Warnf("[ExternalDialect] account=%s could not persist the chat fallback: %v",
			accountLabel(account), err)
		return
	}
	// The pool hands out its own copy of the account, so the persisted value
	// only reaches the next request once the pool re-reads the config.
	pool.GetPool().Reload()
}
