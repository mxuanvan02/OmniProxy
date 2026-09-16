# Phase 02 — 403 classification (stop banning on unverified messages)

## Problem (evidence)

On 2026/09/15 the chat path got `403 PERMISSION_DENIED {"error":{"code":403,
"message":"capture","status":"PERMISSION_DENIED"}}`. That message matches none of
`antigravityBanPhrases` / `antigravityAuthPhrases` / `antigravityValidationPhrases`,
so `classifyAntigravityFailure` fell through to `antigravityFailureBanned` and
called `markAntigravityBanned` — a **terminal** state that switches the account
off. Logged as `disabled upstream`.

But both accounts are `banStatus=ACTIVE` **now** (config.json:64,65) and later
answered control-plane calls (`needs owner verification` appeared 09/15 13:21).
So `"capture"` was transient/recoverable in practice — the terminal ban was wrong.

## Current code

`proxy/external_antigravity.go:222-239` (the `http.StatusForbidden` case):
```go
case http.StatusForbidden:
	for _, phrase := range antigravityAuthPhrases {
		if strings.Contains(lower, phrase) {
			return antigravityFailureAuth
		}
	}
	// A bare PERMISSION_DENIED ... is how the disable shows up when no
	// explanatory message is attached.
	return antigravityFailureBanned
```
Any 403 without an auth phrase → banned. Including a 403 that *carries an
unrecognized message* like `"capture"`.

## Principle

Ban only on what we can **verify** is terminal:
- an explicit ban phrase, OR
- a **bare/empty** PERMISSION_DENIED (the documented disable-with-no-message case).

A 403 that carries a **non-empty message we do not recognize** is not verifiably
terminal — treat it as recoverable (`antigravityFailureOther`), which the callers
already handle by returning the error and rotating the account *without* banning.
This mirrors the existing SSE-truncation philosophy: do not over-punish on
upstream-side ambiguity.

## Change

`proxy/external_antigravity.go`, `classifyAntigravityFailure`, the `StatusForbidden` case:
```go
case http.StatusForbidden:
	for _, phrase := range antigravityAuthPhrases {
		if strings.Contains(lower, phrase) {
			return antigravityFailureAuth
		}
	}
	// A bare PERMISSION_DENIED (no explanatory message) is how Google shows a
	// terminal disable, so keep banning on it. But a 403 that carries a message
	// we do not recognize — e.g. {"message":"capture"} — is not verifiably
	// terminal: it has been observed to clear on its own. Recover (rotate the
	// account, no ban) instead of switching it off on an unknown string.
	if antigravityBarePermissionDenied(lower) {
		return antigravityFailureBanned
	}
	return antigravityFailureOther
```

New helper beside the phrase lists:
```go
// antigravityBarePermissionDenied reports whether a 403 body is the messageless
// PERMISSION_DENIED Google returns for a terminal disable. A body that names any
// other message is treated as unrecognized-but-not-terminal by the caller.
func antigravityBarePermissionDenied(lower string) bool {
	// "message" empty/absent, or message == the bare status string.
	if !strings.Contains(lower, "permission_denied") {
		return false
	}
	// Recognized non-terminal markers are handled elsewhere; here we only need to
	// tell "no real message" from "some message we don't know".
	return !antigravityHasDistinctMessage(lower)
}
```
`antigravityHasDistinctMessage` parses the JSON body and reports whether
`error.message` is a non-empty string that is not just the status echoed back.
(Implementation: small `json.Unmarshal` into `struct{ Error struct{ Message string } }`;
non-empty and `!strings.EqualFold(message,"permission_denied")` → distinct.)

> Keep it simple: if JSON parse fails, fall back to the old behavior (treat as
> bare → banned), so a malformed body never silently downgrades a real ban.

## Caller behavior (no change needed)

- Chat path (`:1172` switch): handles Banned/Auth/Validation; `Other` falls
  through to `return fmt.Errorf(...)` → caller rotates, no ban. Correct.
- Generate path (`:577` switch): same — `Other` returns the error, retries stop,
  no ban. Correct.

So returning `antigravityFailureOther` for unknown-message 403s already yields
"recoverable, rotate, don't ban" with no caller edits.

## Tests

Extend `TestClassifyAntigravityFailure`:
- `{"error":{"code":403,"message":"capture","status":"PERMISSION_DENIED"}}` → `antigravityFailureOther` (was Banned).
- `{"error":{"code":403,"status":"PERMISSION_DENIED"}}` (no message) → `antigravityFailureBanned` (unchanged).
- `{"error":{"code":403,"message":"PERMISSION_DENIED","status":"PERMISSION_DENIED"}}` (message echoes status) → `antigravityFailureBanned`.
- `{"error":{"code":403,"message":"account has been disabled"}}` → `antigravityFailureBanned` (ban phrase wins).
- `{"error":{"code":403,"message":"Verify your account to continue.",...VALIDATION_REQUIRED}}` → `antigravityFailureValidation` (unchanged).
- malformed/unparseable 403 body → `antigravityFailureBanned` (safe fallback).

New `TestAntigravityUnknown403DoesNotBan`: drive the chat/generate path with a
fake upstream returning the `"capture"` body; assert `account.BanStatus != "BANNED"`
after the call (mirror the existing `TestAntigravityValidationDoesNotBan` harness).

## Success criteria
- `capture` and other unknown-message 403s no longer ban.
- Real bans (explicit phrase, bare PERMISSION_DENIED) still ban.
- Validation path unchanged. All existing tests pass.

## Risks
- If Google ever uses an unknown message for a *real* terminal disable, we now
  rotate instead of ban → a few wasted retries on a dead account. Acceptable:
  the account is re-evaluated each request, and the observed `"capture"` case was
  recoverable. The bare-PERMISSION_DENIED path still catches messageless disables.
