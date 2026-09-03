# OmniProxy — Codebase Hardening & Refactor

**Created:** 2026-09-02 19:28 · **Source:** 4 parallel code-review reports (architecture/DRY, security, tests/CI/frontend, concurrency/correctness)
**Baseline:** `go build` ✓ · `go vet` ✓ · `go test -race ./...` ✓ (exit 0) · 138 Go files / 59.4k LOC · coverage: proxy 42.7%, pool 59.9%, config 40.2%, auth 15.0%

## Findings summary

| Sev | Count | Highlights |
|-----|-------|-----------|
| CRITICAL | 5 | `Save()` unlocked → concurrent map read/write panic; pool hands out `*Account` mutated lock-free; default `0.0.0.0` + `changeme` + auth off; admin pw via `?pwd=`; wildcard CORS on admin API |
| HIGH | 8 | No client-context propagation (abandoned streams hold accounts 15min); compression globals raced; no `recover()` in 8 goroutines; pool write-lock for reads; world-readable credential writes (0644); SSRF via jina-reader; CI runs no Go verification; auth pkg 15% covered |
| MED | 11 | UTC/local chart mismatch; `flushToDisk` drops errors + clears dirty on failure; unbounded `io.ReadAll` body; stale-snapshot stat loss; root Docker + floating base; no CSP; duplicated `escapeHtml` |
| LOW | 9 | dead code (5 funcs); 2MB unused `icon.svg`; misnamed `*_real_test.go`; 1 missing locale key ×2; repo noise 54MB |

Clean (verified, no action): PKCE both flows · resp.Body closure at every `Do()` · lock ordering pool→config, no inversion · no secret ever in git history (167 commits) · no unescaped `innerHTML` found · monotonic-clock cooldowns.

## Phases

| # | Phase | Status | Files owned | Parallel group |
|---|-------|--------|-------------|----------------|
| 01 | [Critical concurrency fixes](phase-01-critical-concurrency.md) | ✅ done — build/vet/`-race` green; C2 as copy-return, not value | `config/config.go`, `pool/account.go`, `proxy/compression.go`, `proxy/account_refresh.go`, `proxy/usage_tracker.go` | **A** |
| 02 | [CI + Docker hardening](phase-02-ci-and-docker.md) | ✅ done — `docker build` **unverified** (daemon down) | `.github/workflows/ci.yml`, `Dockerfile`, `docker-compose.yml` | **A** |
| 03 | [Frontend & hygiene](phase-03-frontend-hygiene.md) | ✅ done — browser click-through **unverified** (no browser) | `web/escape.js`, `web/quota.js`, `web/locales/*.json`, `web/icon.svg`, `_backups/` | **A** |
| 04 | [Security hardening](phase-04-security.md) | ✅ done — dashboard click-through **unverified** (no browser) | `proxy/handler.go` (auth/CORS/perms), `proxy/admin_session.go`, `proxy/urlguard.go`, `proxy/auth.go`, `config/apikeys.go`, `proxy/search.go`, `config/config.go`, `main.go`, `cli/cli.go`, `web/app.js`, `web/logs.js`, `web/usage.js`, `web/settings.js` | B (after 01) |
| 05 | [Correctness & resource limits](phase-05-correctness.md) | ✅ done — manual disconnect test **unverified** (no live upstream account) | `proxy/handler.go`, `proxy/responses_handler.go`, `proxy/kiro.go`, `proxy/kiro_api.go`, `proxy/external_*.go`, `proxy/usage_tracker.go`, `auth/autoimport.go`, `main.go`, `cli/cli.go` | C (after 04) |
| 06 | [Test coverage gaps](phase-06-test-coverage.md) | 🟡 partial — auth 15→50.2%, pool 59.9→88.2%, cli 0→22.8%; `config` untouched, `TestMain` decoupling + `backgroundRefresh` not done | `auth/*_test.go`, `pool/*_test.go`, `cli/lockfile_test.go`, `proxy/*_test.go` (new files) | C (after 04) |
| 07 | [handler.go decomposition + DRY](phase-07-decomposition-dry.md) | ⬜ deferred | `proxy/handler_*.go`, `proxy/sse_frames.go` | D (after 05+06) |

**Execution:** group A (01+02+03) concurrent — disjoint files. Then 04. Then 05+06 concurrent. 07 deferred (pure refactor, 11.8k-line file; needs failover tests from 06 landed first).

## Key dependencies

- 01 changes pool selector signatures (`*Account` → value) → 06's pool tests must follow 01.
- 04 rewrites admin auth → 03 must not touch `web/app.js`; 04 owns all three JS auth call sites.
- `?pwd=` cannot simply be deleted: `EventSource` (`web/logs.js:168`, `web/usage.js:215`) cannot set headers. 04 replaces it with a single-use session token, not the password.
- 07 must diff against `_backups/refactor-wip-20260901-111329/` (5,396 lines of an abandoned identical split) before writing new code.

## Code review (post-implementation)

`code-reviewer` pass over the full diff. 10 findings; 7 fixed, 3 recorded.

| # | Sev | Finding | Disposition |
|---|-----|---------|-------------|
| 1 | HIGH | ctx propagation made a client disconnect surface as `context canceled` from every account in turn; no classifier matched it, so the failover loop walked the pool, recorded a failure against each account and could model-lock it pool-wide. **Regression introduced by phase 05.** | fixed — `clientGone(ctx, err)` in `proxy/account_failover.go`, bail before `handleAccountFailure` at all 6 `dispatchChat` failover sites. Pinned by `proxy/client_disconnect_test.go` (verified to fail against the unfixed code) |
| 2 | HIGH | `writeJSONAtomic` used a fixed `path+".tmp"`; `Stop()` can flush concurrently with `periodicFlush`'s ticker → two writers splice one temp file → `loadFromDisk` silently discards all usage history | fixed — `os.CreateTemp` with a unique name, matching `config.Save` |
| 3 | MED-HIGH | changing the admin password left every outstanding session token valid for its full 12 h | fixed — `adminSessions.revokeAll()` on a password change |
| 4 | MED-HIGH | `apiUpdateCompressionConfig` held the compression write lock across up to 6 fsynced config saves; phase 01 turned readers into `RLock` holders, so one PATCH stalled every in-flight request | fixed — mutate under the lock, persist after releasing it |
| 5 | MED | phase 01's M4 fix missed the Codex branch: `markCodexAuthFailure` still wrote the whole record via `UpdateAccountPreservingCredentials`, resetting counters live requests had bumped | fixed — `UpdateAccountBanStatus` |
| 6 | MED | copy-return means `GetByID` callers that wrote through the old pointer no longer reach the pool (antigravity project discovery, Codex account-id extraction) | fixed for the antigravity refresh endpoint (`pool.Reload()`); the Codex lazy extraction is bounded by the 40+ existing `Reload()` sites and left alone |
| 7 | MED | the SSE `?token=` credential is the full 12 h session token, not the "single-use token" this plan's Key dependencies specify | **not fixed — known deviation.** Narrower than `?pwd=` (no longer the permanent password) but still leaks a 12 h admin credential into access logs and history. Follow-up: stream-scoped short TTL |
| 8 | LOW | `isWeakPassword` and `validatePassword` disagreed; `{"password":" "}` set a one-space secret that `ensurePasswordLocked` would not repair | fixed — one trimmed rule, `validatePassword` delegates to `isWeakPassword` |
| 9 | LOW | the SSRF guard validates `target`, but the request goes to Jina with the target as a path suffix; DNS is resolved twice (rebinding TOCTOU) and no `CheckRedirect` is set | **not fixed — recorded.** Only bites when an admin points `BaseURL` at a local reader, which phase 04's Decisions deliberately allows |
| 10 | LOW | `idx := i` is dead ceremony under Go 1.22+ per-iteration loop vars | fixed |

Reviewer's "unresolved" item — whether `refreshAntigravityAccountToken` persists the rotated token — checked: it ends in `config.UpdateAccountToken` (`proxy/external_antigravity.go:133`). No finding.

Verified clean by the review: lock order pool→config never inverted, no missed unlock paths, `safeGo`+`WaitGroup` panic-safe, `flushToDisk` dirty-flag handling correct, ctx propagation complete, no admin route reachable without a token, token comparisons constant-time and length-blind.

## Deferred / rejected

- **Provider `interface`** — rejected. `dispatchChat` 5-branch switch already works; gommo media funcs have incompatible shapes.
- **Unified retry helper across providers** — rejected. 2 call sites, different semantics (onboarding poll vs job poll).
- **`<200 lines/file` rule** — relaxed to ~400–700 for Go HTTP handlers; single admin endpoints legitimately run 200–500 lines.
