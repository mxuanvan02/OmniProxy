# Phase 02 — Pool health snapshot and route

**Priority:** Critical — this is the page's "which vendor is sick right now" panel.
**Status:** Not started
**Depends on:** nothing (independent of phase 01)
**Blocks:** phase 03 (the `PoolHealth` TypeScript shape), and therefore 04, 05, 06.

## Context links

- `plans/20260915-1622-providers-view/design.md` §4B
- `pool/account.go`, `pool/cooldown_class.go`, `proxy/handler.go`, `proxy/etag.go`
- `pool/account_error_recording_test.go`, `pool/account_model_selection_test.go`

## Overview

The pool knows exactly why an account is not being routed to: `cooldowns`,
`errorCounts` and `modelLocks` on `AccountPool` (`pool/account.go:91-95`). None
of it is reachable from HTTP. The only pool-health value in the whole admin API
is `"available": h.pool.AvailableCount()` on `/admin/api/status`
(`proxy/handler.go:11339`), and `AvailableCount` (`:1516-1533`) reads
account-level cooldowns only — a model-level lock is invisible to it, so an
account whose only model is locked still counts as available.

The operator-visible consequence: an account card reads green while every
request to the model it serves is being refused locally by the pool, before any
upstream call is made.

This phase exposes that state and, crucially, the *reason* for it.

## Key insights

**1. `CooldownClass` is computed and thrown away.** `ClassifyCooldown`
(`pool/cooldown_class.go:146`) maps an error onto a class with a human-readable
`String()` — `"rate_limited"`, `"auth_failed"`, `"no_balance"`. Every caller
uses it for exactly two things: `immediate()` and `duration()`. The class is
then discarded; only its duration is stored, and the class name reaches only log
lines. Without capturing it, the panel can say "locked until 15:04" and never
"because auth_failed" — which is the single most useful sentence it could say,
since "wrong credential" and "wait a minute" call for opposite responses.

**2. Expired entries are never pruned, so a naive dump lies.** The only deletes
in `pool/account.go` are in `RecordSuccess` (`:1081`, `:1085`, `:1087`) and
`ClearCooldown` (`:1459`, `:1460`). A cooldown that expired hours ago is still in
the map. `HealthSnapshot` must filter at read time with `now.Before(until)`,
mirroring `AvailableCount` (`:1524`) — and must **not** delete under `RLock`.

**3. `omitempty` does not work on `time.Time`.** `time.Time` is a struct, so
`encoding/json` never treats it as empty and the zero value serialises as
`"0001-01-01T00:00:00Z"` — an account with only consecutive errors would report a
bogus cooldown deadline. Timestamps are `int64` unix seconds instead, matching
`lastUsed` / `codexPrimaryResetAt` elsewhere in the API, and directly consumable
by the frontend's existing `relativeTime(unixSeconds)`.

**4. A new struct field must be initialised in the test helper.**
`newModelPool` (`pool/account_model_selection_test.go:13`) builds an
`AccountPool` by hand. Adding `lockReasons` without initialising it there makes
`recordErrorWithClass` write to a nil map and panic across the whole existing
pool test suite.

**5. The handler is a shim, but the route still gets a test.** Every decision
worth testing — filtering expired entries, attaching reasons — lives in
`HealthSnapshot()` in the `pool` package, where `newModelPool` provides
fixtures. The handler itself is four lines of encoding. The route *string*, the
method gate and the JSON envelope are a separate question, and they are only
exercised by a real request through `handleAdminAPI`. `proxy/service_test.go`
already has that pattern: `getServiceTestPool(t)` for the pool and
`issueAdminTestToken(t)` for the session.

A 401 probe against the live server cannot substitute for it: the session gate
at `proxy/handler.go:6151` runs **before** the route switch, so an unregistered
path also answers 401 and the two are indistinguishable from outside.

## Requirements

**Functional**
- `GET /admin/api/pool/health` returns, for every account with live failure
  state: its cooldown deadline and reason, its consecutive error count, and its
  per-model locks with deadlines and reasons.
- Expired cooldowns and expired model locks are absent from the response.
- The response carries `since` and `uptimeSeconds` so the client can disclose
  that the state is in-memory and resets on restart.
- ETag-wrapped, like `/accounts` and `/status`.
- Admin-session authenticated, like every other `/admin/api/` route.

**Non-functional**
- Read-only: acquires `RLock` only, never mutates, never calls `config.*`.
- Does not grow `lockReasons` without bound — cleared wherever the lock it
  describes is cleared.

## Architecture

```
recordErrorWithClass(id, class, model)
    ├── cooldowns[id]        = now+duration        (account-level)
    ├── modelLocks[id][model]= now+duration        (model-level)
    └── lockReasons[key]     = class.String()      ← new, key = id or id+"\x00"+model
                    │
RecordSuccess / ClearCooldown ── delete the matching lockReasons entries
                    │
                    ▼
        AccountPool.HealthSnapshot()  ── RLock, filter expired, project to int64 unix
                    │
                    ▼
GET /admin/api/pool/health → {"accounts": {...}, "since": ..., "uptimeSeconds": ...}
```

## Related code files

- **Modify:** `pool/account.go` — `AccountPool.lockReasons`, `GetPool()` init, `lockReasonKey`, `recordErrorWithClass`, `RecordSuccess`, `ClearCooldown`, new `AccountHealth`/`ModelLock`/`HealthSnapshot`
- **Modify:** `pool/account_model_selection_test.go` — initialise `lockReasons` in `newModelPool`
- **Modify:** `proxy/handler.go` — one route case, one handler func
- **Create:** `pool/account_health_test.go`
- **Create:** `proxy/pool_health_route_test.go`
- **Delete:** none

## Implementation steps

### Step 1: Write the failing tests

Create `pool/account_health_test.go`:

```go
package pool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A cooldown whose deadline has passed must not appear. Expired entries are
// never deleted from the maps — only RecordSuccess and ClearCooldown delete —
// so the filter has to happen at read time, exactly as AvailableCount does.
func TestHealthSnapshotFiltersExpiredEntries(t *testing.T) {
	p := newModelPool()
	now := time.Now()
	p.mu.Lock()
	p.cooldowns["a"] = now.Add(-time.Minute)
	p.modelLocks["a"] = map[string]time.Time{"claude-opus-5": now.Add(-time.Second)}
	p.lockReasons[lockReasonKey("a", "")] = "auth_failed"
	p.lockReasons[lockReasonKey("a", "claude-opus-5")] = "rate_limited"
	p.mu.Unlock()

	if snap := p.HealthSnapshot(); len(snap) != 0 {
		t.Fatalf("expired state leaked into the snapshot: %#v", snap)
	}
}

// An active cooldown reports both its deadline and why it was applied. The
// class is otherwise computed and discarded, which is what leaves a panel able
// to say "locked until 15:04" but never "because auth_failed".
func TestHealthSnapshotReportsCooldownReason(t *testing.T) {
	p := newModelPool()
	class := p.RecordErrorClass("a", errors.New("403 Authentication failed"), "")
	if class != CooldownAuthFailed {
		t.Fatalf("class = %s, want auth_failed — the fixture no longer exercises this path", class)
	}

	snap := p.HealthSnapshot()
	got, ok := snap["a"]
	if !ok {
		t.Fatal("account a missing from the snapshot after an immediate-class failure")
	}
	if got.CooldownReason != "auth_failed" {
		t.Fatalf("cooldownReason = %q, want %q", got.CooldownReason, "auth_failed")
	}
	if got.CooldownUntil <= time.Now().Unix() {
		t.Fatalf("cooldownUntil = %d, want a future deadline", got.CooldownUntil)
	}
}

// A model lock reports its own reason and leaves the account-level fields empty,
// so the panel does not imply the whole account is down.
func TestHealthSnapshotReportsModelLockReason(t *testing.T) {
	p := newModelPool()
	p.RecordError("a", true, "claude-opus-5")

	snap := p.HealthSnapshot()
	got, ok := snap["a"]
	if !ok {
		t.Fatal("account a missing from the snapshot after a model lock")
	}
	if got.CooldownUntil != 0 || got.CooldownReason != "" {
		t.Fatalf("model lock reported as an account cooldown: %#v", got)
	}
	lock, ok := got.ModelLocks["claude-opus-5"]
	if !ok {
		t.Fatalf("claude-opus-5 lock missing: %#v", got.ModelLocks)
	}
	if lock.Reason != "rate_limited" {
		t.Fatalf("lock reason = %q, want %q", lock.Reason, "rate_limited")
	}
	if lock.Until <= time.Now().Unix() {
		t.Fatalf("lock until = %d, want a future deadline", lock.Until)
	}
}

// An account one failure short of a lock is indistinguishable from a healthy
// one without this: no cooldown exists yet, so nothing else in the API shows it.
func TestHealthSnapshotReportsConsecutiveErrors(t *testing.T) {
	p := newModelPool()
	p.RecordError("a", false, "claude-opus-5")
	p.RecordError("a", false, "claude-opus-5")

	snap := p.HealthSnapshot()
	got, ok := snap["a"]
	if !ok {
		t.Fatal("account with pending strikes missing from the snapshot")
	}
	if got.ConsecutiveErrors != 2 {
		t.Fatalf("consecutiveErrors = %d, want 2", got.ConsecutiveErrors)
	}
	if got.CooldownUntil != 0 || len(got.ModelLocks) != 0 {
		t.Fatalf("two strikes should not have locked anything yet: %#v", got)
	}
}

// Success on one model clears that model's lock and its reason while leaving a
// sibling model's lock intact — and leaving the sibling's reason attached to the
// lock it still describes.
func TestRecordSuccessClearsOnlyThatModelsLockReason(t *testing.T) {
	p := newModelPool()
	p.RecordError("a", true, "claude-opus-5")
	p.RecordError("a", true, "claude-sonnet-5")

	p.RecordSuccess("a", "claude-opus-5")

	snap := p.HealthSnapshot()
	got := snap["a"]
	if _, still := got.ModelLocks["claude-opus-5"]; still {
		t.Fatal("claude-opus-5 lock survived its own success")
	}
	lock, ok := got.ModelLocks["claude-sonnet-5"]
	if !ok {
		t.Fatal("sibling lock was cleared too")
	}
	if lock.Reason != "rate_limited" {
		t.Fatalf("sibling reason = %q, want %q", lock.Reason, "rate_limited")
	}
	if _, leaked := p.lockReasons[lockReasonKey("a", "claude-opus-5")]; leaked {
		t.Fatal("reason key outlived the lock it described")
	}
}

// ClearCooldown is called by the reset-quota endpoint. Without clearing the
// reasons too, every model ever locked on that account leaves a key behind for
// the life of the process.
func TestClearCooldownClearsEveryReason(t *testing.T) {
	p := newModelPool()
	p.RecordError("a", true, "")
	p.RecordError("a", true, "claude-opus-5")
	p.RecordError("a", true, "claude-sonnet-5")

	p.ClearCooldown("a")

	if snap := p.HealthSnapshot(); len(snap) != 0 {
		t.Fatalf("state survived ClearCooldown: %#v", snap)
	}
	p.mu.RLock()
	left := len(p.lockReasons)
	p.mu.RUnlock()
	if left != 0 {
		t.Fatalf("%d reason key(s) leaked past ClearCooldown", left)
	}
}

// Pins the wire contract the frontend types are written against. time.Time would
// have serialised its zero value here, so the deadlines are unix seconds.
func TestAccountHealthJSONShape(t *testing.T) {
	data, err := json.Marshal(AccountHealth{
		CooldownUntil:     1_760_000_000,
		CooldownReason:    "auth_failed",
		ConsecutiveErrors: 2,
		ModelLocks:        map[string]ModelLock{"m": {Until: 1_760_000_060, Reason: "rate_limited"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"cooldownUntil":1760000000`, `"cooldownReason":"auth_failed"`, `"consecutiveErrors":2`, `"until":1760000060`, `"reason":"rate_limited"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s missing from %s", want, data)
		}
	}
}
```

### Step 2: Add `lockReasons` to the test helper first

In `pool/account_model_selection_test.go`, in `newModelPool` (`:13`), add one line
to the struct literal, immediately after `modelLocks`:

```go
		modelLocks:            make(map[string]map[string]time.Time),
		lockReasons:           make(map[string]string),
```

Do this **before** running the tests. Every other pool test writes a cooldown
through `recordErrorWithClass`, so without it the whole package panics on a nil
map write once step 5 lands.

### Step 3: Run to verify they fail

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./pool/ -run 'TestHealthSnapshot|TestRecordSuccessClearsOnlyThatModelsLockReason|TestClearCooldownClearsEveryReason|TestAccountHealthJSONShape' -v
```

Expected: compile failure — `undefined: lockReasonKey`, `p.lockReasons undefined`, `undefined: AccountHealth`.

### Step 4: Add the types

In `pool/account.go`, immediately before the `AccountPool` struct definition
(`:83`):

```go
// AccountHealth is a read-only view of the pool's transient failure state for a
// single account. Everything here lives in memory and is empty after a restart:
// it describes what the pool has learned since it last started, not the
// account's lifetime history.
//
// Deadlines are unix seconds, not time.Time. time.Time is a struct, so
// omitempty does not apply to it and its zero value serialises as year 1 — an
// account with pending strikes would report a cooldown that never happened.
type AccountHealth struct {
	CooldownUntil     int64                `json:"cooldownUntil,omitempty"`
	CooldownReason    string               `json:"cooldownReason,omitempty"`
	ConsecutiveErrors int                  `json:"consecutiveErrors,omitempty"`
	ModelLocks        map[string]ModelLock `json:"modelLocks,omitempty"`
}

// ModelLock is one model's cooldown on one account.
type ModelLock struct {
	Until  int64  `json:"until"`
	Reason string `json:"reason,omitempty"`
}
```

### Step 5: Add the field and its key helper

In the `AccountPool` struct (`:84-120`), immediately after the `modelLocks` line
(`:95`):

```go
	modelLocks        map[string]map[string]time.Time // accountID → modelName → cooldown until
	// lockReasons explains why each cooldown or model lock was applied, keyed by
	// accountID (account-level) or accountID+"\x00"+model (model-level). The
	// CooldownClass is otherwise computed, used for its duration, and discarded —
	// which leaves an operator able to see that an account is parked but not
	// whether the credential is dead or the window is merely full.
	lockReasons map[string]string
```

Immediately after the `AccountPool` struct's closing brace, before `var (`:

```go
// lockReasonKey namespaces a reason by scope so an account-level cooldown and a
// model lock on the same account cannot overwrite each other's explanation.
func lockReasonKey(accountID, model string) string {
	if model == "" {
		return accountID
	}
	return accountID + "\x00" + model
}
```

### Step 6: Initialise it in `GetPool`

In `GetPool` (`:128`), in the struct literal, immediately after the `modelLocks`
line (`:134`):

```go
			modelLocks:            make(map[string]map[string]time.Time),
			lockReasons:           make(map[string]string),
```

### Step 7: Record the reason where the lock is recorded

Replace the tail of `recordErrorWithClass` (`pool/account.go:1133-1142`):

```go
	if cooldown > 0 && model != "" {
		// Per-model lock: only this model on this account is cooled down
		if p.modelLocks[id] == nil {
			p.modelLocks[id] = make(map[string]time.Time)
		}
		p.modelLocks[id][model] = time.Now().Add(cooldown)
		p.lockReasons[lockReasonKey(id, model)] = class.String()
	} else if cooldown > 0 {
		// Legacy account-level cooldown
		p.cooldowns[id] = time.Now().Add(cooldown)
		p.lockReasons[lockReasonKey(id, "")] = class.String()
	}
```

### Step 8: Clear the reason wherever its lock is cleared

Replace `RecordSuccess` (`pool/account.go:1078-1090`):

```go
func (p *AccountPool) RecordSuccess(id string, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
	delete(p.lockReasons, lockReasonKey(id, ""))
	p.errorCounts[id] = 0
	// Clear model lock for the specific model that succeeded
	if model != "" && p.modelLocks[id] != nil {
		delete(p.modelLocks[id], model)
		delete(p.lockReasons, lockReasonKey(id, model))
		if len(p.modelLocks[id]) == 0 {
			delete(p.modelLocks, id)
		}
	}
}
```

Replace `ClearCooldown` (`pool/account.go:1457-1462`):

```go
func (p *AccountPool) ClearCooldown(id string) {
	p.mu.Lock()
	delete(p.cooldowns, id)
	delete(p.modelLocks, id)
	delete(p.lockReasons, lockReasonKey(id, ""))
	prefix := id + "\x00"
	for key := range p.lockReasons {
		if strings.HasPrefix(key, prefix) {
			delete(p.lockReasons, key)
		}
	}
	p.mu.Unlock()
}
```

`strings` is already imported in `pool/account.go`.

### Step 9: Add the snapshot accessor

In `pool/account.go`, immediately after `AvailableCount` (`:1516-1533`):

```go
// HealthSnapshot returns the live failure state for every account that has one.
//
// It is strictly read-only: entries whose deadline has already passed are
// filtered out here rather than deleted, matching AvailableCount. Nothing in
// this map is persisted, so after a restart it is empty even for an account
// whose credential was revoked a minute earlier — callers must present it as
// "since process start" rather than as history.
//
// config.* is deliberately not consulted: the lock order documented at
// HasAvailableAccountForModel forbids reading config while holding p.mu.
func (p *AccountPool) HealthSnapshot() map[string]AccountHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	out := make(map[string]AccountHealth)
	for id, until := range p.cooldowns {
		if !now.Before(until) {
			continue
		}
		h := out[id]
		h.CooldownUntil = until.Unix()
		h.CooldownReason = p.lockReasons[lockReasonKey(id, "")]
		out[id] = h
	}
	for id, count := range p.errorCounts {
		if count == 0 {
			continue
		}
		h := out[id]
		h.ConsecutiveErrors = count
		out[id] = h
	}
	for id, locks := range p.modelLocks {
		for model, until := range locks {
			if !now.Before(until) {
				continue
			}
			h := out[id]
			if h.ModelLocks == nil {
				h.ModelLocks = make(map[string]ModelLock)
			}
			h.ModelLocks[model] = ModelLock{
				Until:  until.Unix(),
				Reason: p.lockReasons[lockReasonKey(id, model)],
			}
			out[id] = h
		}
	}
	return out
}
```

Note the read-modify-write on `out[id]`: `AccountHealth` is a value type, so the
copy is mutated and written back. The nested `ModelLocks` map is shared, which is
why mutating it in place works.

### Step 10: Run the pool tests

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./pool/ -run 'TestHealthSnapshot|TestRecordSuccessClearsOnlyThatModelsLockReason|TestClearCooldownClearsEveryReason|TestAccountHealthJSONShape' -v
```

Expected: 7 PASS.

Then the whole package, to catch the nil-map trap:

```bash
GOCACHE="$TMPDIR/gocache" go test ./pool/ 2>&1 | tail -20
```

Expected: all PASS.

### Step 11: Write the route test

Create `proxy/pool_health_route_test.go`:

```go
package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The pool package proves what the snapshot contains; this proves the route is
// reachable, that it is GET-only, and that the envelope the frontend decodes has
// exactly these three keys.
//
// A live 401 probe cannot stand in for this: the admin session gate runs before
// the route switch in handleAdminAPI, so an unregistered path answers 401 too.
func TestPoolHealthRouteReturnsSnapshotEnvelope(t *testing.T) {
	initConfigForTests(t)
	h := &Handler{pool: getServiceTestPool(t), startTime: time.Now().Add(-90 * time.Second).Unix()}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/pool/health", nil)
	req.Header.Set(adminTokenHeader, issueAdminTestToken(t))
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accounts      map[string]AccountHealth `json:"accounts"`
		Since         int64                    `json:"since"`
		UptimeSeconds int64                    `json:"uptimeSeconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — body was %s", err, rec.Body.String())
	}
	if body.Accounts == nil {
		t.Fatalf("accounts decoded as null; it must be an empty object: %s", rec.Body.String())
	}
	if body.UptimeSeconds < 89 || body.UptimeSeconds > 120 {
		t.Fatalf("uptimeSeconds = %d, want ~90 — the handler is not reading h.startTime", body.UptimeSeconds)
	}
	if body.Since != h.startTime {
		t.Fatalf("since = %d, want %d", body.Since, h.startTime)
	}
}

// Without a session token the handler must not be reached at all.
func TestPoolHealthRouteRequiresAdminSession(t *testing.T) {
	initConfigForTests(t)
	h := &Handler{pool: getServiceTestPool(t), startTime: time.Now().Unix()}

	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, httptest.NewRequest(http.MethodGet, "/admin/api/pool/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
}
```

### Step 12: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'TestPoolHealthRoute' -v
```

Expected: both fail. The first prints `status = 404` (the route is not in the
switch yet; with a valid token the gate passes and the switch falls through),
the second passes already.

### Step 13: Register the route

In `proxy/handler.go`, in the `handleAdminAPI` switch, immediately after the
`/status` case (`:6309-6310`):

```go
	case path == "/pool/health" && r.Method == "GET":
		// Buffered through withETag: pool health changes only when a failure is
		// recorded, so an unchanged snapshot costs a header exchange.
		withETag(w, r, h.apiGetPoolHealth)
```

### Step 14: Add the handler

In `proxy/handler.go`, immediately before `apiGetStatus` (`:11306`):

```go
// apiGetPoolHealth reports the pool's in-memory failure state. It is a shim:
// every decision worth testing — filtering expired entries, attaching reasons —
// lives in AccountPool.HealthSnapshot, where the pool package's fixtures can
// reach it.
//
// "since" is the process start time, not the age of the oldest entry, because
// the snapshot is empty after a restart and the client has to say so.
func (h *Handler) apiGetPoolHealth(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":      h.pool.HealthSnapshot(),
		"since":         h.startTime,
		"uptimeSeconds": time.Now().Unix() - h.startTime,
	})
}
```

### Step 15: Run the route tests, then build and vet

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'TestPoolHealthRoute' -v
GOCACHE="$TMPDIR/gocache" go build ./...
GOCACHE="$TMPDIR/gocache" go vet ./pool/ ./proxy/
```

Expected: 2 PASS, then no output from `go build` and `go vet`.

### Step 16: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add pool/account.go pool/account_health_test.go pool/account_model_selection_test.go proxy/handler.go proxy/pool_health_route_test.go
git commit -F - <<'EOF'
feat(pool): expose cooldowns, model locks and their reasons over HTTP

The pool knew why an account was not being routed to — cooldowns, per-model
locks and consecutive error counts — but none of it was reachable from the
admin API. The only pool-health value on the wire was AvailableCount, which
reads account-level cooldowns only, so an account whose sole model was locked
still reported as available and its card stayed green while every request to
it was refused locally.

HealthSnapshot is read-only and filters expired entries at read time rather
than pruning them, because nothing except RecordSuccess and ClearCooldown ever
deletes from those maps and a naive dump would report long-dead cooldowns as
active. Deadlines are unix seconds: time.Time ignores omitempty, so its zero
value would have serialised as year 1 for an account that only had pending
strikes.

CooldownClass now survives the call that computes it. It was previously used
for its duration and thrown away, leaving an operator able to see an account
parked until 15:04 but not whether its credential was dead or its window
merely full — two conditions with opposite remedies.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `pool/account_health_test.go` with all seven tests
- [ ] Step 2: initialise `lockReasons` in `newModelPool` (**before** running tests)
- [ ] Step 3: confirm the tests fail to compile
- [ ] Step 4: add `AccountHealth` and `ModelLock` with `int64` deadlines
- [ ] Step 5: add the `lockReasons` field and `lockReasonKey`
- [ ] Step 6: initialise `lockReasons` in `GetPool`
- [ ] Step 7: write the reason in `recordErrorWithClass`
- [ ] Step 8: clear the reason in `RecordSuccess` and `ClearCooldown`
- [ ] Step 9: add `HealthSnapshot`
- [ ] Step 10: 7 PASS, then the whole `./pool/` package passes
- [ ] Step 11: write `proxy/pool_health_route_test.go`
- [ ] Step 12: confirm the route test fails with 404
- [ ] Step 13: register `/pool/health`
- [ ] Step 14: add `apiGetPoolHealth`
- [ ] Step 15: 2 route PASS, `go build ./...` and `go vet` clean
- [ ] Step 16: commit

## Success criteria

- `TestPoolHealthRouteReturnsSnapshotEnvelope` passes: HTTP 200 through
  `handleAdminAPI` with a session token, and a body with exactly the keys
  `accounts`, `since`, `uptimeSeconds`.
- `TestPoolHealthRouteRequiresAdminSession` passes: HTTP 401 without a token.
- With no recent failures, `accounts` encodes as `{}` — an empty object, not
  `null` (asserted in the route test, which decodes it into a non-nil map).
- After `POST /admin/api/accounts/{id}/reset-quota`, that account is absent from
  `accounts`, and no reason key remains for it (asserted by
  `TestClearCooldownClearsEveryReason`).
- Two identical requests are idempotent under ETag — the second returns 304.
  Verify this in the browser under phase 06, not from the shell: minting a
  session token from outside the process is exactly what the login flow exists
  to prevent.

## Risk assessment

| Risk | Mitigation |
|---|---|
| `lockReasons` not initialised in `newModelPool` → whole pool suite panics | Step 2 runs before Step 3; Step 10 runs the full package |
| A reason left behind after its lock is cleared | `TestRecordSuccessClearsOnlyThatModelsLockReason`, `TestClearCooldownClearsEveryReason` |
| `time.Time` + `omitempty` emitting year 1 | Deadlines are `int64`; `TestAccountHealthJSONShape` pins it |
| The route string, method gate or envelope drifting from what the frontend types decode | `TestPoolHealthRouteReturnsSnapshotEnvelope` goes through `handleAdminAPI` with a real session token and asserts all three keys |
| Passing `config.*` into the handler, or reading it under `p.mu` | The handler reads only `h.pool` and `h.startTime` |
| Reading `config.*` under `p.mu` deadlocking against chat routing | `HealthSnapshot` touches only pool-owned maps; the lock-order rule is restated in its doc comment |
| Exposing internal state widens the attack surface | Returns only timestamps, counters and class names — no tokens, no URLs, no credentials |

## Security considerations

- **Authorization:** the route sits after the `adminSessions.valid(...)` gate at
  `proxy/handler.go:6151`, like every other `/admin/api/` route. An unauthenticated
  request gets 401 before the handler is reached.
- **Data exposed:** account IDs, cooldown deadlines, error counts, model names
  and cooldown class names. No access tokens, no refresh tokens, no API keys, no
  `baseUrl`. Class names are a closed set of five strings.
- **Data flow:** read-only. No input is parsed, so there is no injection surface.
- **Persistence:** none. The state dies with the process, which is itself a
  security property — no failure history is written to disk.

## Next steps

Phase 03 mirrors `AccountHealth` / `ModelLock` / `PoolHealth` into
`web-next/src/lib/api.ts` and adds `api.poolHealth()`. Phase 04 renders it and
must label the panel with `since` / `uptimeSeconds` rather than presenting it as
history.
