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
