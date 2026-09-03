package pool

import (
	"omniproxy/config"
	"testing"
	"time"
)

// RecordError only cools an account down on the third consecutive non-quota
// error. Cooling down on the first would take an account out of rotation for a
// single transient blip.
func TestRecordErrorLocksModelOnlyAfterThreeConsecutiveErrors(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})

	for i := 1; i <= 2; i++ {
		p.RecordError("a", false, "claude-opus-5")
		if got := p.GetNextForModel("claude-opus-5"); got == nil {
			t.Fatalf("account was locked after %d non-quota error(s), want it still routable", i)
		}
	}

	p.RecordError("a", false, "claude-opus-5")
	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("account still routable after 3 consecutive errors, got %q", got.ID)
	}
}

// A quota error cools the model down immediately: retrying a 429 costs the
// request and cannot succeed until the window resets.
func TestRecordErrorQuotaLocksModelImmediatelyForAnHour(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "claude-opus-5")

	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("quota error did not lock the model, got %q", got.ID)
	}

	p.mu.RLock()
	until := p.modelLocks["a"]["claude-opus-5"]
	p.mu.RUnlock()
	if remaining := time.Until(until); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("quota lock lasts %s, want about 1h", remaining)
	}
}

// Per-model locking is the point of the model argument: a quota failure on one
// model must not take the account's other models out of rotation.
func TestRecordErrorLocksOnlyTheFailingModel(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "claude-opus-5")

	if got := p.GetNextForModel("claude-sonnet-5"); got == nil || got.ID != "a" {
		t.Fatalf("sibling model was locked too: %#v", got)
	}
}

// An empty model falls back to an account-level cooldown, which blocks every
// model on that account.
func TestRecordErrorWithEmptyModelSetsAccountLevelCooldown(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "")

	p.mu.RLock()
	_, hasAccountCooldown := p.cooldowns["a"]
	modelLocks := len(p.modelLocks["a"])
	p.mu.RUnlock()

	if !hasAccountCooldown {
		t.Fatal("empty model did not set an account-level cooldown")
	}
	if modelLocks != 0 {
		t.Fatalf("account-level cooldown also created %d model locks", modelLocks)
	}
	if got := p.GetNextForModel("any-model"); got != nil {
		t.Fatalf("account-level cooldown did not block routing, got %q", got.ID)
	}
}

// RecordSuccess must both clear the cooldown and reset the consecutive-error
// counter. Leaving the counter armed would cool the account down again after
// one further error instead of three.
func TestRecordSuccessClearsCooldownAndResetsErrorCounter(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	for i := 0; i < 3; i++ {
		p.RecordError("a", false, "")
	}
	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatalf("precondition: account should be cooled down, got %q", got.ID)
	}

	p.RecordSuccess("a", "")

	if got := p.GetNextForModel("claude-opus-5"); got == nil {
		t.Fatal("RecordSuccess did not clear the account cooldown")
	}
	p.mu.RLock()
	count := p.errorCounts["a"]
	p.mu.RUnlock()
	if count != 0 {
		t.Fatalf("error counter = %d after success, want 0", count)
	}

	// Counter reset means it takes three fresh errors to lock again.
	for i := 1; i <= 2; i++ {
		p.RecordError("a", false, "")
		if got := p.GetNextForModel("claude-opus-5"); got == nil {
			t.Fatalf("account locked again after only %d post-success error(s)", i)
		}
	}
}

func TestRecordSuccessClearsOnlyTheNamedModelLock(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "claude-opus-5")
	p.RecordError("a", true, "claude-sonnet-5")

	p.RecordSuccess("a", "claude-opus-5")

	if got := p.GetNextForModel("claude-opus-5"); got == nil || got.ID != "a" {
		t.Fatalf("succeeded model is still locked: %#v", got)
	}
	if got := p.GetNextForModel("claude-sonnet-5"); got != nil {
		t.Fatalf("success on one model cleared another model's lock, got %q", got.ID)
	}
}

// The per-account lock map must not be left behind empty once its last entry is
// cleared, otherwise the map grows once per model per account for the process
// lifetime.
func TestRecordSuccessRemovesEmptyModelLockMap(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "claude-opus-5")
	p.RecordSuccess("a", "claude-opus-5")

	p.mu.RLock()
	_, exists := p.modelLocks["a"]
	p.mu.RUnlock()
	if exists {
		t.Fatal("empty model-lock map was left behind after the last lock was cleared")
	}
}

// An expired lock must stop filtering on its own — routing recovers without any
// explicit success being recorded.
func TestExpiredModelLockNoLongerBlocksRouting(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.mu.Lock()
	p.modelLocks["a"] = map[string]time.Time{"claude-opus-5": time.Now().Add(-time.Second)}
	p.mu.Unlock()

	if got := p.GetNextForModel("claude-opus-5"); got == nil || got.ID != "a" {
		t.Fatalf("expired model lock still filtered the account: %#v", got)
	}
}

func TestExpiredAccountCooldownNoLongerBlocksRouting(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.mu.Lock()
	p.cooldowns["a"] = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if got := p.GetNextForModel("claude-opus-5"); got == nil || got.ID != "a" {
		t.Fatalf("expired account cooldown still filtered the account: %#v", got)
	}
}

// ClearCooldown is what the reset-quota admin action uses; it must drop both the
// account cooldown and every per-model lock, or the account stays unroutable
// after the operator was told it was reset.
func TestClearCooldownRemovesAccountCooldownAndModelLocks(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "claude-opus-5")
	p.RecordError("a", true, "")

	p.ClearCooldown("a")

	for _, model := range []string{"claude-opus-5", "claude-sonnet-5"} {
		if got := p.GetNextForModel(model); got == nil || got.ID != "a" {
			t.Fatalf("model %q still blocked after ClearCooldown: %#v", model, got)
		}
	}
}

// AvailableCount reports unique accounts, not weighted slice entries, and must
// exclude those in cooldown.
func TestAvailableCountExcludesCooledDownAccountsAndDeduplicates(t *testing.T) {
	p := newModelPool(
		config.Account{ID: "a"},
		config.Account{ID: "a"}, // weight 2 → two slice entries, one account
		config.Account{ID: "b"},
	)
	if got := p.AvailableCount(); got != 2 {
		t.Fatalf("AvailableCount = %d, want 2 unique accounts", got)
	}

	p.RecordError("b", true, "")
	if got := p.AvailableCount(); got != 1 {
		t.Fatalf("AvailableCount = %d after cooling down b, want 1", got)
	}
}

// A per-model lock is not an account-level cooldown: AvailableCount only tracks
// the latter, so an account locked for one model still counts as available.
func TestAvailableCountIgnoresPerModelLocks(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordError("a", true, "claude-opus-5")

	if got := p.AvailableCount(); got != 1 {
		t.Fatalf("AvailableCount = %d, want 1 (a model lock is not an account cooldown)", got)
	}
}

func TestMarkOverLimitCoolsAccountDownForAnHour(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(config.Account{ID: "a"})

	p.MarkOverLimit("a")

	p.mu.RLock()
	until, ok := p.cooldowns["a"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("MarkOverLimit did not set a cooldown")
	}
	if remaining := time.Until(until); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("over-limit cooldown lasts %s, want about 1h", remaining)
	}
}
