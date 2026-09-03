# Phase 01 — Critical concurrency fixes

**Context:** review reports (concurrency/correctness). Parallel group **A**. Priority **P0**. Status ✅ done.

**Deviation:** C2 returns a *copy* behind the existing `*config.Account` signature, not a value. Full value-return changes nil-check semantics at ~15 call sites in files owned by sibling phases; the spec permitted "by value **or a copy**". Two sites the review missed were also fixed: `GetNextForCapability`, `GetByID`'s second loop over `serviceAccounts`.

## Overview

Live crash risks + silent data corruption under concurrent request traffic. Race detector currently passes only because no test drives parallel `GetNext*` + `ensureValidToken` + `UpdateToken`.

## Key insights

- `go test -race ./...` exit 0 today = weak evidence; the racy paths are untested.
- Lock order is consistently pool→config; do not invert while fixing.
- Selector paths are *inconsistent*: strategy path returns `&preferredKnown[0]` / `pickByStrategy` (locals, safe), direct path returns `&p.accounts[idx]` (shared, racy). Unify on value returns.

## Requirements

Functional: no behaviour change to routing decisions, token refresh, or persisted config shape.
Non-functional: `go test -race ./...` must still pass; no new lock-order edges.

## Related code files

Modify: `config/config.go`, `pool/account.go`, `proxy/compression.go`, `proxy/account_refresh.go`, `proxy/usage_tracker.go`, and the `ensureValidToken` / selector call sites in `proxy/handler.go` that the signature change touches.
Create: none. Delete: none.

## Implementation steps

1. **C1 — `Save()` called without `cfgLock`.** `config/config.go:1012-1013` (`SetBoolSetting`) and `:1028-1029` (`SetStringSetting`) unlock then `_ = Save()`. `Save()` marshals `cfg` unlocked and `preserveNewerRuntimeFields` reads `cfg.*` + writes package global `saveBaseline`. Concurrent `SetStringSetting` (holding the lock, writing `cfg.KVSettings[k]`) vs the marshal = `concurrent map read and map write` → unrecoverable runtime throw, whole proxy dies.
   Fix: keep `cfgLock` held across `Save()`, matching the other call sites (`:1150`, `:1502`). If `Save()` itself acquires the lock, add an unlocked `saveLocked()` and call that.
2. **C2 — pool returns pointers into `p.accounts`.** `pool/account.go:376`, `:396`, `:652`, `:716`, `:955` return `&p.accounts[i]` after `RUnlock`. Callers then write through it: `proxy/handler.go:5229-5234`, `:5243`, `:2178`, `:2549`, `:2669` set `account.AccessToken` / `ExpiresAt` while `pool.UpdateToken` writes the same fields under `p.mu.Lock` and other selectors read them under `RLock`. Unsynchronised string-header write vs read → 401 on a healthy account or crash. `Reload()` (`:194`) replaces the whole slice, so an in-flight pointer writes into an orphaned array and the token update is lost from memory.
   Fix: return `config.Account` **by value** (or a copy) from every selector; have callers persist token updates via `pool.UpdateToken` / `config.UpdateAccount` rather than by mutating a shared struct. Verify no call site depends on pointer identity.
3. **H2 — compression globals raced.** Writers hold `mu.Lock` (`proxy/compression.go:435-460`); readers take nothing (`:78`, `:296`, `:299`, `:319`, `:344`, `:350`, `:52-57`). Torn string read on a live `cavemanLevel` switch → wrong prompt or OOB panic; same for `headroomURL`.
   Fix: add a locked snapshot accessor returning a value struct; call it once at request entry and pass the local down.
4. **H4 — write lock for read-only selection.** `pool/account.go:264-265` takes `p.mu.Lock()` then calls `config.GetAllowOverUsage()` (`:270`) which grabs `cfgLock.RLock` while holding `p.mu` — fully serialises all capability routing against chat routing.
   Fix: `RLock`, and hoist the `config` read out of the critical section.
5. **H3 — no `recover()` in spawned goroutines.** `grep -c recover` over non-test Go = 0. Goroutines: `proxy/usage_tracker.go:415`, `proxy/account_refresh.go:301`, `proxy/handler.go:7213`, `:7509`, `:9824`, `:1145`, `:1147`, `:1149`. `net/http` only recovers its own handler goroutines; a nil-deref in bulk refresh exits the process and drops every in-flight stream.
   Fix: one shared `safeGo(name string, fn func())` helper with `defer recover()` + `logger.Errorf`; convert the 8 sites.
6. **M4 — stale-snapshot stat loss.** `proxy/account_refresh.go:292-347` spawns a goroutine per account over a `config.GetAccounts()` snapshot (`:293`), each calling `config.UpdateAccount(id, *account)` (`:183`, `:206`, `:246`) which replaces the record wholesale — clobbering counters a live request bumped via `UpdateAccountStats`.
   Fix: switch those three writes to a field-scoped patch (token/ban fields only), leaving stats untouched.
7. **M2 — `flushToDisk` swallows errors, clears dirty on failure, mutates under `RLock`.** `proxy/usage_tracker.go:289-309`: `os.WriteFile` errors discarded (`:304`, `:307`) and `t.dirty = false` at `:309` runs regardless → a full disk silently discards usage records permanently; non-atomic write truncates `usage_daily.json` on a mid-write crash.
   Fix: log the error, keep `dirty` set on failure, write temp+rename like `config.Save`, and take the write lock for the flag.
8. Run `go build ./... && go vet ./... && go test -race ./...` after each numbered step, not only at the end.

## Todo

- [x] C1 `Save()` under `cfgLock` — `saveLocked()` + `defer Unlock`
- [x] C2 selectors return a copy (`selected := *acc`), signature kept `*config.Account`; 7 sites
- [x] H2 `snapshot()` accessor; `InitCompressionConfig` + stats API now locked
- [x] H4 `RLock` + `GetAllowOverUsage()` hoisted above the lock (2 selectors)
- [x] H3 `safeGo` helper in `usage_tracker.go`; 12 sites converted
- [x] M4 `config.UpdateAccountBanStatus` — 4 wholesale writes replaced
- [x] M2 `flushToDisk`: `writeJSONAtomic` temp+rename 0600, errors logged, `dirty` restored on failure, write lock for the flag
- [x] build / vet / `-race` green

## Success criteria

`go test -race ./...` passes. No `&p.accounts[` returned to callers. No `recover`-less `go ` statement outside the http handler tree. `Save()` never reachable without `cfgLock` held.

## Risk assessment

- C2 is the widest blast radius: touches ~40 `Reload()` sites indirectly and every selector caller. Mitigation: change one selector at a time, compile between each; do not batch.
- Holding `cfgLock` across `Save()` lengthens the critical section (temp file + `Sync` + `Rename`). Acceptable — correctness over latency — but note it, and do not also hold `p.mu`.
- H3's `recover()` must log and not silently swallow; a swallowed panic is worse than a crash for diagnosis.

## Security considerations

Token fields are what C2 corrupts; a torn `AccessToken` read can send a malformed Authorization header upstream. No new credential surface introduced by this phase.

## Next steps

Blocks phase 06 (pool tests must target the post-C2 signatures). Phase 04 follows this phase because both touch `proxy/handler.go`.
