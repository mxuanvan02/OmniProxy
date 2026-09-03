# Phase 06 — Test coverage gaps

**Context:** [plan.md](plan.md) · source: tests/CI review (HIGH 2/3/4/5, MED 9/10, LOW 13/15)
**Priority:** HIGH · **Status:** 🟡 mostly done — `config` coverage + `TestMain` decoupling + `backgroundRefresh` smoke test not done · **Group:** C — concurrent with phase 05, after phase 04

## Overview

Measured coverage: `auth` 15.0%, `config` 40.2%, `pool` 59.9%, `proxy` 42.7%, `logger` 72.7%, `main`/`cli` 0.0%. The uncovered set is not the boring tail — it is every OAuth refresh path, the model-aware pool selector, and the CLI-config writer that touches real files under `$HOME`.

## Key insights

- `auth/oidc.go` (24 KB) has **no test file at all**.
- `pool.GetNextForModel` (`pool/account.go:507`) is 0% — the model-filtered selector is the one routing actually uses. Base `GetNext` (`:333`) is 100%, `IsAuthFailure` (`:1014`) 100%, stickiness 90–100%. So rotation and error classification are pinned; model filtering and `RecordSuccess` (`:967`) are not.
- Phase 01 changes selector signatures (`*Account` → value). **Write pool tests against the post-01 shape**, not the current one.
- `config` shows 40.2% line coverage but **85 functions at 0%** → branch coverage is materially worse than the number suggests.
- No `TestMain` anywhere; 4 files mutate package/process globals (`config/config_test.go`, `proxy/service_test.go`, `proxy/handler_models_test.go`, `pool/strategy_test.go`). `t.Parallel` count is 0, so there is no live bug — but that is also why parallelism can't be added.
- Tests make **zero real network calls** (verified). `*_real_test.go` is a misnomer: `cache_real_test.go` / `usage_real_test.go` are pure unit tests. Rename, don't rewrite.
- CLI-config functions write to `~/.hermes/…`-class paths. Tests must redirect `HOME` (`t.Setenv`) — never let a test touch the real one.

## Requirements

Functional — no production code changes in this phase except the two renames and any bug the new tests expose (fix those in the owning phase's file, coordinating with 05).
Non-functional — `go test -race ./...` green; no test depends on execution order; no test performs network I/O or writes outside `t.TempDir()`.

## Architecture

`httptest.Server` for every token endpoint; table-driven cases per failure mode (200 valid, 200 malformed, 401, 500, timeout, empty body). `t.TempDir()` + `t.Setenv("HOME", …)` for anything filesystem-facing.

## Related code files

Create
- `auth/oidc_test.go` — first tests for the file
- `auth/antigravity_oauth_test.go` — refresh/exchange/decode
- `auth/autoimport_test.go` — sqlite import against a fixture DB
- `auth/builderid_test.go`, `auth/iam_sso_test.go`
- `proxy/handler_cli_config_test.go` — CLI-tool config writers, `HOME` redirected
- `cli/lockfile_test.go` — stale-lock detection

Modify
- `pool/account_test.go` — add `GetNextForModel`, `RecordSuccess`, `CountAccountsForModel`, `GetByID`, `PruneCacheWarmed`
- `proxy/cache_real_test.go` → `proxy/cache_test.go`; `proxy/usage_real_test.go` → `proxy/usage_semantics_test.go`
- the 4 global-mutating test files — add fixtures or `TestMain` so order no longer matters

## Implementation steps

1. **H2 auth refresh paths.** Start with `RefreshAntigravityToken` (`antigravity_oauth.go:293`) and `postAntigravityToken` (`:313`) against an `httptest.Server`: valid refresh, expired refresh token, 401, malformed JSON, network error. Then `exchangeAntigravityCode` (`:275`), `DecodeGoogleIDToken` (`:372`), `PollCodexLogin` (`codex_oauth.go:283`), `StartBuilderIdLogin` (`builderid.go:32`), `PollBuilderIdAuth` (`:150`), `StartIamSsoLogin`/`CompleteIamSsoLogin` (`iam_sso.go:44`,`:100`). These are the functions whose silent failure 401s every request.
2. **`auth/oidc.go`.** Read it first — it has no tests, so the contract is undocumented. Cover discovery + token handling with a stubbed OIDC endpoint.
3. **H3 pool.** `GetNextForModel` under: no accounts, all cooled down, all quota-exhausted (with and without `allowOverUsage`), one healthy among banned, model supported by a subset. Then `RecordSuccess` and the bookkeeping funcs. Target the post-phase-01 value-returning signatures.
4. **H4 CLI config writers.** `backupToolConfig` (`handler.go:574`), `getToolConfigPaths` (`:646`), `checkToolInstalled` (`:687`), `checkToolHasOmniProxy` (`:706`), `getCliToolsStatus` (`:894`) with `t.Setenv("HOME", t.TempDir())`. Assert the backup is created, the original is not corrupted, and — after phase 04 — that the mode is 0600.
5. **H5 background refresh.** `backgroundRefresh` (`handler.go:1211`) and `NewHandler` (`:1117`). At minimum: the loop exits on its stop channel (which phase 05 wires up) and a refresh failure does not kill the process (phase 01's `safeGo`).
6. **`autoimport.go`.** `readSQLite` (`:87`), `ImportSSOCache` (`:211`) against a fixture sqlite file in `t.TempDir()`. Include the `SQLITE_BUSY` case phase 05 fixes.
7. **MED 9 global state.** Give the 4 files either a `TestMain` that snapshots/restores the config global, or per-test fixtures. Then add `t.Parallel()` where it is now safe — optional, but it proves the decoupling worked.
8. **LOW 13/15 renames + cli.** Rename the two `*_real_test.go` files. Add `cli/lockfile_test.go` for `lockfile.go:70` — a stale-lock false positive makes the binary refuse to start, which is a silent-until-user-reports failure.
9. Re-measure: `go test -cover ./...`. Record before/after per package in this file.

## Todo

- [x] `auth` refresh/exchange/decode covered (antigravity, builderid, iam_sso)
- [x] `auth/oidc.go` has tests — `oidc_dispatch_test.go`, `oidc_refresh_test.go`, `oidc_profile_test.go`
- [x] `pool.GetNextForModel` + `RecordSuccess` + bookkeeping covered
- [x] CLI-config writers covered with `HOME` redirected — `proxy/handler_cli_config_test.go`
- [ ] `backgroundRefresh` / `NewHandler` smoke-covered — **not done**
- [x] `autoimport` sqlite paths covered — `auth/autoimport_sqlite_test.go` (phase 05 agent)
- [x] `-count=2` order-independence fixed — `auth.ResetRotationMapForTest()` added and called by `handler_token_refresh_test.go` + `import_credentials_test.go`; the 60s rotation-map TTL was making a second run reuse its own first-run cache and skip the refresh it asserts on. No `TestMain` added; the 4 config-global files still share state but no longer fail.
- [x] `*_real_test.go` renamed → `cache_usage_flow_test.go`, `usage_aggregation_test.go`
- [x] `cli/lockfile` covered — `cli/lockfile_test.go`
- [x] coverage re-measured (below)

## Coverage measured

| Package | Before | After | Target | |
|---------|--------|-------|--------|---|
| `auth` | 15.0% | **50.2%** | ≥50% | ✅ |
| `pool` | 59.9% | **88.2%** | ≥75% | ✅ |
| `config` | 40.2% | **40.5%** | ≥55% | ❌ untouched |
| `proxy` | 42.7% | **44.6%** | — | |
| `cli` | 0.0% | **22.8%** | — | |

`config` is the miss: no new tests were written for it, so the 85-functions-at-0% finding stands. Carried to phase 07 follow-ups.

## Success criteria

- `auth` ≥ 50%, `pool` ≥ 75%, `config` ≥ 55%; every named function above has at least one test.
- `go test -race ./...` green, and green again when run twice in a row (order independence).
- No test writes outside `t.TempDir()`; `grep` for real hostnames in new tests returns only string literals.
- CI (phase 02) runs all of it.

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| Pool tests written against pre-01 signatures → immediate rewrite | 06 starts only after 01 has landed; read `pool/account.go` before writing |
| New tests expose real bugs, scope creeps | Fix in the owning phase's file and note it here; do not silently widen this phase |
| CLI-config tests touch the real `$HOME` | `t.Setenv("HOME", t.TempDir())` in every such test; review before commit |
| Chasing a coverage number instead of behaviour | Targets are floors, not goals. A test that only calls a function to colour the line is worse than none |

## Security considerations

Test fixtures must use obviously fake credentials (`test-token-xxx`), never a real token copied from `data/config.json`. No fixture may be a real OAuth response body with live values.

## Next steps

Unblocks phase 07 — the failover tests here are what make extracting `withAccountRetry` safe.
