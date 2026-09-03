# Phase 07 — handler.go decomposition + DRY

**Context:** [plan.md](plan.md) · source: architecture/DRY review
**Priority:** MED · **Status:** ⬜ deferred · **Group:** D — after phases 05 + 06

## Overview

`proxy/handler.go` is 11,844 lines / 235 functions. Four copies of the account-failover loop mean a pool-selection fix must be applied four times; three copies of the SSE scan loop have already drifted apart into different bugs. Deferred deliberately: pure refactor, and it needs phase 06's failover tests landed first.

## Key insights

- **An abandoned refactor already did this split and was reverted.** `_backups/refactor-wip-20260901-111329/` holds 7 files / 5,396 lines with symbols identical to current `handler.go` (e.g. backup `handler_9router_import.go:18,49,195` = `handler.go:8700,8731,8877`; backup `handler_cli_api.go:14,549,626,1101` = `handler.go:5578,6114,6191,6666`). `git log` shows zero commits for those paths and `_backups/` is gitignored. **Diff the backups against current `handler.go` before writing a single new line** — most of the work may already exist. Phase 03 must not delete that directory.
- Why it was abandoned is unknown (conflict? regression? interrupted?). Establish that before choosing resume-vs-restart.
- `<200 lines/file` is unrealistic here: `apiApplyCliToolSettings` alone is 536 lines (`handler.go:5578-6113`) and is legitimately one endpoint. Target ~400–700 per file.
- All extractions are same-package pure moves: no signature changes, no call-site edits.
- Comments in the moved code explain *why* (`handler.go:3143-3145` on why post-output retry is unsafe; `external_openai.go:744-746` on EOF inline errors). Move them verbatim.

## Requirements

Functional — zero behaviour change. Byte-identical responses.
Non-functional — `handler.go` under ~1,500 lines; `-race` green; no import cycles.

## Architecture

| # | New file | Extract from `handler.go` | ~LOC | Risk |
|---|----------|---------------------------|------|------|
| 1 | `handler_cli_config.go` | 319-955 (`setCliToolSettings`…`getCliToolsStatus`, `cliBackupWriter`, `upsertYAMLProviderBlock:384`, `upsertYAMLModelSection:439`) | 640 | none |
| 2 | `handler_cli_api.go` | 5578-6689 (`apiApplyCliToolSettings`, `apiResetCliToolSettings`, `parseTOML:6191`, `readCliToolSettingsFromFile:6222`) | 1,110 | none |
| 3 | `handler_login_api.go` | 7590-8699 (IAM SSO / BuilderID / Kiro SSO / social / Codex OAuth) | 1,110 | none |
| 4 | `handler_account_import.go` | 9240-10316 (Kiro CLI / token / apikey / external-provider / SSO-cache / credentials import) | 1,080 | none |
| 5 | `handler_models.go` | 1618-2960 + 98-318 (`refreshModelsCache:1894`, `mergeUniqueModels:2682`, `modelTokenLimits:2794`, OpenClaw/hermes YAML blocks) | 1,340 | low |
| 6 | `handler_account_api.go` | 2162-2681 + 7082-7589 + 10500-11184 (refresh/reset/quota/overage/batch/test/models-per-account) | 1,540 | low |
| 7 | `handler_mitm.go` | 6788-6971 + `mitmToolHosts:6906` | 190 | none |
| 8 | `handler_stream_claude.go` / `handler_stream_openai.go` | 2994-4280 / 4281-4993 | 1,290 / 710 | **med — hot path, do last** |

Residual `handler.go` ≈ 1,400 lines: `Handler` struct, `ServeHTTP:1398`, `handleAdminAPI:5265` (the 313-line route table — keep intact, it *is* the router), token refresh/retry (`ensureValidToken:5143`, `tryRefreshAndRetry:4994`, `tryTransientRetry:5076`), stats/usage recording (3712-4034).

## Related code files

Create: the 9 files above, plus `proxy/sse_frames.go`.
Modify: `proxy/handler.go` (removals), `proxy/external_openai.go`, `external_codex.go`, `external_antigravity.go`, `proxy/images.go` (SSE callers), `proxy/external_openai.go:1238` (delete `dispatchCodex`).
Delete: dead functions listed below.

## Implementation steps

1. **Diff the backups first.** For each of the 7 files in `_backups/refactor-wip-20260901-111329/`, diff bodies (not just line offsets) against current `handler.go`. Confirm whether they snapshot current code or a pre-0.4.0 state — offsets match, bodies unverified. Record the answer here.
2. **Moves 1–4** (zero coupling to the request path, highest LOC removed). One file per commit; `go build && go vet && go test -race ./...` between each.
3. **Moves 5–7.**
4. **DRY: SSE scan loop.** `parseExternalOpenAISSE:677`, `parseCodexResponsesSSE:745`, `parseAntigravitySSE:893` each reimplement nil-callback guard, `bufio.NewReaderSize`, watchdog Start/Stop/TimedOut, `ReadString('\n')`, `TrimRight "\r\n"`, `data:` prefix strip, `[DONE]`, EOF-final-line, terminal-event check. They have already drifted: codex lacks the `blankOutputGate` that openai (`:681`) and antigravity (`:899`) have; antigravity lacks the EOF inline-error extraction openai has at `:747-754` specifically to enable AgentRouter fallback. Extract `func scanSSEFrames(body io.Reader, bufSize int, onData func(string) error) error`; each parser keeps its own `onData` (envelope shapes genuinely differ) and its own terminal check. 4 callers incl. `images.go:409`. **Decide whether codex's missing gate is deliberate** (Responses API may guarantee non-blank) before unifying — do not silently change behaviour.
5. **DRY: call prelude.** `CallExternalOpenAI:51-113`, `CallExternalAntigravity:1005-1069`, `callAgentRouterOpenAIRequest:96`, `CallExternalCodex:287` share: nil-account guard → trim BaseURL/token → marshal → `NewRequest` → headers → `GetClientForProxy(ResolveAccountProxyURL(…))` → non-200 → `ReadAll` + `truncateErrBody` → Content-Type sniff → SSE-vs-JSON branch. Extract only the tail: `postAndParse(account, req, sse, jsonParse)`. Error *classification* stays per-provider — `classifyCodexAuthFailure:133` and `classifyAntigravityFailure:173` do genuinely different things.
6. **DRY: the real one — handler failover loop ×4.** `handleClaudeStream:3112`, `handleOpenAIStream:4363`, `handleClaudeNonStream:4042`, `handleOpenAINonStream:4826` each run the same `for attempt := 0; ; attempt++` body: log attempt + `h.pool.Count()` + excluded → select-or-bail → `ensureValidToken` → skip-cache-tracker-for-external → `attemptProduced` output gate → `dispatchChat` → `tryRefreshAndRetry`/`tryTransientRetry`. Only the emit differs (SSE vs buffered). Extract `func (h *Handler) withAccountRetry(model string, payload *KiroPayload, mk func(*config.Account) *KiroStreamCallback) error`. **Do this after move 8**, and only with phase 06's failover tests green.
7. **Dead code.** `KiroToOpenAIResponse` (`translator.go:2170`, superseded by `…WithReasoning`); `InitUsageTracker:861` + `GetTracker:865` (`usage_tracker.go`, `GetUsageTracker` is the live accessor); `UsageTracker.SetErrorProvider:870`; `config.UpdateSettings` (`config.go:1519` — dead, `handler.go:10434` calls `UpdateSettingsPatch`); `dispatchCodex` (`external_openai.go:1238` — redundant `isCodexAccount` check `dispatchChat:1343` already does). Each: confirm no non-test callers, then delete with its tests.
8. **`Config.RequireApiKey`** (`config.go:390`) is self-labelled `[Deprecated]` yet still plumbed through `handler.go:10423` and `auth.go:41-45`. A deprecated field on the auth path is a hazard — either finish the removal or drop the marker. Decide, don't leave it.
9. **Do not** introduce a provider `interface` or unify provider retry. Both rejected in review: `dispatchChat`'s 5-branch switch already works and gommo's media funcs have incompatible shapes; the two retry loops have different semantics.

## Todo

- [ ] Backups diffed; resume-or-restart decided and recorded
- [ ] Moves 1–4 landed, build+race green after each
- [ ] Moves 5–7 landed
- [ ] `scanSSEFrames` extracted, 4 callers migrated, codex gate question answered
- [ ] `postAndParse` extracted
- [ ] Moves 8 (stream files) landed
- [ ] `withAccountRetry` extracted, 4 call sites migrated
- [ ] 5 dead functions deleted
- [ ] `RequireApiKey` resolved either way
- [ ] `handler.go` ≤ ~1,500 lines

## Success criteria

`handler.go` ≈ 1,400 lines, no file over ~1,600. One SSE scan loop, one failover loop. `go test -race ./...` green. Responses byte-identical before/after — diff a captured stream from each provider.

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| Silent behaviour change in the hot path | Moves are pure; the two *behavioural* extractions (4, 6) come last and require 06's tests. Capture and diff real streams |
| Redoing work that already exists in `_backups/` | Step 1 is mandatory and blocks everything else |
| Unifying SSE parsers propagates one provider's bug to all three | Keep `onData` + terminal checks per-provider; only the transport loop is shared |
| Huge diff, unreviewable | One move per commit, build+race between each. Never batch |
| Deleting a "dead" function that has a reflective/dynamic caller | `grep` the whole repo incl. web assets before each deletion |

## Security considerations

`handler_login_api.go` (move 3) and `handler_account_import.go` (move 4) carry the OAuth and credential-import paths. Move them verbatim — resist "while I'm here" edits. If phase 04 changed file modes in that range, rebase onto it rather than reconciling by hand.

## Next steps

Terminal phase. On completion, re-run the four reviewers against the new layout.
