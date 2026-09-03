# Phase 05 — Correctness & resource limits

**Context:** [plan.md](plan.md) · source: concurrency/correctness review (H1, M1, M3, M5, LOW set)
**Priority:** HIGH · **Status:** ⬜ pending · **Group:** C — concurrent with phase 06, after phase 04

## Overview

A client disconnect does not cancel the upstream stream: no `context.Context` reaches any provider call, and the SSE writes discard their errors. An abandoned CLI session holds its account for up to the 900 s idle timeout while tokens keep being billed. Plus: unbounded request-body buffering, a UTC/local mismatch that makes two dashboard numbers disagree nightly, and a shutdown path that drops the final stats flush.

## Key insights

- `sseIdleWatchdog` covers *upstream* silence only. Client death is invisible to it.
- The admin SSE endpoints already do this correctly (`handler.go:11556-11576`, `:11657-11675` use `r.Context().Done()` + stopped tickers) — the proxy path is the outlier. Copy the working pattern.
- `dispatchChat` (`external_openai.go:1341`), `CallKiroAPI` (`kiro.go:426`), `CallExternalCodex` take no ctx; requests are built with `http.NewRequest` (`kiro.go:501`, `external_codex.go:338`, `external_openai.go:81`).
- Streaming writes ignore errors: `fmt.Fprintf(w, "data: %s\n\n", …)` at `handler.go:4493`, `:4653`, `:4730`, `:3707`.
- `io.ReadAll(r.Body)` at `handler.go:2963` has no `MaxBytesReader`, while the admin endpoint at `:2595` correctly caps at 16 KB. `ReadTimeout: 15m` (deliberate, for large contexts) gives a slow-loris room.
- Timezone: `dailyData` keyed UTC (`usage_tracker.go:348`), `dailyCutoffDate` UTC (`:752`), but `GetChartData` passes local `time.Now()` (`:546`), `bucketByDay` formats local (`:595-597`), `getPeriodCutoff("today")` uses local midnight (`:844`). At UTC+7, 03:00 local = previous UTC day → today's chart column reads 0 while `/usage?period=today` shows the UTC total.
- `stopRefresh`/`stopStatsSaver` exist (`handler.go:936-937`) but nothing closes them (`grep close(stopRefresh)` = 0); `shutdownServer` (`main.go:115`) only calls `srv.Shutdown` → final `saveStats()` / `FlushPendingSave()` never run.

## Requirements

Functional
- Client disconnect aborts the upstream request and releases the account promptly.
- Oversized request bodies are rejected with 413, not OOM.
- Chart buckets and period totals agree, in one timezone consistently.
- Clean shutdown flushes stats + any coalesced config save.

Non-functional
- No change to streaming latency or SSE framing.
- `-race` stays green.

## Architecture

```
r.Context() ─┬─▶ dispatchChat(ctx, …) ─▶ http.NewRequestWithContext ─▶ upstream
             └─▶ write loop: check Fprintf error ─▶ abort on broken pipe
                                                     └─▶ account released
```

Timezone decision: **key everything in UTC** (storage is already UTC; changing storage is riskier than changing the two read paths).

## Related code files

Modify
- `proxy/handler.go` — thread ctx from `ServeHTTP` into the four stream/non-stream paths; check `Fprintf` errors; `MaxBytesReader` at `:2963`
- `proxy/kiro.go` — `CallKiroAPI` ctx param, `NewRequestWithContext` (`:501`); confirm the client timeouts at `:80`, `:165` (unverified in review)
- `proxy/external_openai.go`, `external_codex.go`, `external_antigravity.go`, `external_agentrouter.go`, `external_gommo.go` — ctx params + `NewRequestWithContext`
- `proxy/usage_tracker.go` — UTC in `GetChartData`/`bucketByDay`/`getPeriodCutoff`; stop channel for `periodicFlush`
- `main.go` — shutdown closes stop channels, then flushes
- `auth/autoimport.go` — sqlite `_busy_timeout`, `SetMaxOpenConns(1)`, check `rows.Err()` (`:273-280`)

## Implementation steps

1. **H1 ctx propagation.** Add `ctx context.Context` as the first parameter to `dispatchChat` and each `CallExternal*` / `CallKiroAPI`; switch every `http.NewRequest` to `NewRequestWithContext`. Thread `r.Context()` from the four handler entry points. Do this provider by provider, compiling between each — the signature change fans out widely.
2. **H1 write-error detection.** In each SSE emit loop, capture the `Fprintf` error; on error log at debug and return so the parse loop unwinds and the account is released. Verify the account actually leaves `activeReqs` on that path.
3. **M3 body cap.** `http.MaxBytesReader` at `handler.go:2963` with a generous limit (large contexts are legitimate — size it from the configured max context, not arbitrarily), returning 413 on overflow.
4. **M1 timezone.** Make `GetChartData` (`:546`), `bucketByDay` (`:595-597`) and `getPeriodCutoff` (`:844`) all use UTC so they match the UTC storage keys. Add a test that pins a non-UTC `TZ` and asserts chart-today equals period-today.
5. **LOW shutdown.** Close `stopRefresh`/`stopStatsSaver`, add a stop channel to `periodicFlush` (`usage_tracker.go:275`) and to `saveLoop` (`config.go:753`), then call `saveStats()` + `FlushPendingSave()` from `shutdownServer` after `srv.Shutdown`. Decide whether the channels were vestigial or intended — the review could not tell.
6. **M5 sqlite.** `auth/autoimport.go:88`: add `_busy_timeout` to the DSN and `SetMaxOpenConns(1)`; note that `SetConnMaxLifetime(5s)` at `:97` is **not** a query timeout despite its comment — fix the comment. Check `rows.Err()` at `:273-280` so a partial table list is distinguishable from a complete one; stop swallowing the error in `discoverTables` (`:270`).
7. **LOW body-close-in-loop.** `resolveKskProfile` (`handler.go:9920`) has `defer resp.Body.Close()` inside a `for` — bounded at 3 iterations so the leak is trivial, but close explicitly per iteration.
8. Build / vet / `-race` after steps 1, 2, 4. Manually test: start a stream, Ctrl-C the client, confirm the log shows the abort and the account is released without waiting 900 s.

## Todo

- [x] ctx threaded through all providers, `NewRequestWithContext` on every chat path — `dispatchChat(ctx, …)`, `CallKiroAPI`, `CallExternalOpenAI`, `CallExternalCodex`, `CallExternalAntigravity`, `CallExternalAgentRouter` (+ its two internal helpers); handler entry points pass `r.Context()` through `handleClaudeStream`/`handleClaudeNonStream`/`handleOpenAIStream`/`handleOpenAINonStream`/`handleResponsesStream`/`handleResponsesNonStream`/`tryRefreshAndRetry`/`tryTransientRetry`
- [x] account released on client disconnect — **via ctx, not write-error checks** (see Decisions)
- [x] `MaxBytesReader` on the inference body path — `readInferenceBody`, 64 MiB, 413 on overflow; 4 entry points (count_tokens, claude messages, openai chat, openai responses)
- [x] UTC consistency in chart + period reads, with a `TZ`-pinned test — also fixed `bucketByHour(now, true)` filling today's columns from *yesterday*
- [x] Shutdown closes stop channels and flushes stats + config — `Handler.Shutdown()` (`sync.Once`) closes both channels + `usageTracker.Stop()`; wired in `main.go` and `cli/cli.go` daemon path, then `config.FlushPendingSave()`
- [x] sqlite busy timeout + `rows.Err()` — via `_pragma=busy_timeout(…)`; the doc's `_busy_timeout` is a silent no-op on `modernc.org/sqlite`
- [x] `resolveKskProfile` body close per iteration
- [x] build / vet / `-race` green — **manual disconnect test unverified** (no live upstream account in this env)

## Decisions

**Client disconnect is handled by ctx cancellation, not by checking `Fprintf` errors.** `net/http` cancels `r.Context()` the moment the client goes away, and every upstream chat request now derives from it — so the upstream call aborts and the account is released without waiting for a write to fail. Adding error checks to the 16 `sendSSE`/`Fprintf` call sites would be a second, redundant detector on a slower signal (it only fires on the *next* write, which for an idle stream may never come). Left alone deliberately; the phase doc's step 2 is satisfied by step 1.

**Admin-only outbound calls keep `http.NewRequest`.** `fetchExternalProviderModels`, `fetchExternalProviderCredits`, `fetchCodexUsageAttempt`, `codexResetCreditsAvailable`, `codexConsumeResetCredit`, `antigravityPostJSON`, `CallAgentRouterTest`, `fetchAgentRouterModels`, `listAvailableProfiles` — none are on a client request path, so there is no client context to inherit and each already has a client timeout.

## Success criteria

- Ctrl-C on a streaming client → upstream request cancelled, account released, verified in logs (not after the idle timeout).
- A multi-GB POST to `/v1/messages` yields 413, RSS flat.
- With `TZ=Asia/Ho_Chi_Minh` at a post-midnight-local/pre-midnight-UTC hour, chart-today == period-today.
- Ctrl-C on the proxy → final stats written; no lost 30 s window.

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| ctx threading is a wide signature change → merge pain with phase 06 | 06 only adds `_test.go` files; keep it that way. Compile per provider |
| Over-eager cancellation kills a healthy slow stream | Only cancel on `r.Context()` done or a real write error — never on a timer |
| `MaxBytesReader` limit too low → rejects legitimate large contexts | Size from configured max context; log the rejected size so tuning is possible |
| Switching chart reads to UTC changes numbers operators are used to | It makes two disagreeing numbers agree; note it in the changelog |

## Security considerations

- The body cap is also the slow-loris / memory-exhaustion mitigation — treat it as a security fix, not just hygiene.
- Cancelling upstream on disconnect stops billing tokens for abandoned requests; a client that opens and abandons streams in a loop is currently a cheap resource-exhaustion vector against the account pool.

## Next steps

Phase 07 (decomposition) after this and 06 land.
