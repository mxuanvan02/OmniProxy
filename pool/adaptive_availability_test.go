package pool

import (
	"omniproxy/config"
	"sync/atomic"
	"testing"
	"time"
)

// The adaptive router probes capacity while ranking candidates, so the probe has
// to be a pure read. If it advanced the round-robin cursor, merely scoring a
// route would skew load distribution for the request that follows.
func TestHasAvailableAccountForModelDoesNotAdvanceRoundRobinCursor(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(config.Account{ID: "probe-a"}, config.Account{ID: "probe-b"})

	before := atomic.LoadUint64(&p.currentIndex)
	for i := 0; i < 10; i++ {
		if !p.HasAvailableAccountForModel("claude-opus-5") {
			t.Fatalf("iteration %d: healthy pool reported no capacity", i)
		}
	}
	if after := atomic.LoadUint64(&p.currentIndex); after != before {
		t.Fatalf("availability probe moved the cursor: %d -> %d", before, after)
	}
}

// "Supported" and "available right now" are different claims. Availability must
// exclude cooled-down, quota-blocked and model-locked accounts, while the
// support count stays high so the adaptive fallback chain can still retain the
// model and let the normal recovery path wait it out.
func TestHasAvailableAccountForModelSeparatesSupportFromCapacity(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(
		config.Account{ID: "cap-cooled"},
		config.Account{ID: "cap-exhausted", UsageCurrent: 5, UsageLimit: 5},
		config.Account{ID: "cap-locked"},
	)
	p.mu.Lock()
	p.cooldowns["cap-cooled"] = time.Now().Add(time.Hour)
	p.modelLocks["cap-locked"] = map[string]time.Time{"claude-opus-5": time.Now().Add(time.Hour)}
	p.mu.Unlock()

	if p.HasAvailableAccountForModel("claude-opus-5") {
		t.Fatal("cooled-down, exhausted and model-locked accounts reported capacity")
	}
	if got := p.CountAccountsForModel("claude-opus-5"); got != 3 {
		t.Fatalf("CountAccountsForModel = %d, want 3 (support must ignore transient state)", got)
	}

	healthy := newModelPool(config.Account{ID: "cap-cooled-2"}, config.Account{ID: "cap-healthy"})
	healthy.mu.Lock()
	healthy.cooldowns["cap-cooled-2"] = time.Now().Add(time.Hour)
	healthy.mu.Unlock()
	if !healthy.HasAvailableAccountForModel("claude-opus-5") {
		t.Fatal("one healthy peer among cooled-down accounts must report capacity")
	}
}

func TestHasAvailableAccountForModelRejectsBlankModelAndNilPool(t *testing.T) {
	initTempPoolConfig(t)
	p := newModelPool(config.Account{ID: "blank-probe"})
	if p.HasAvailableAccountForModel("   ") {
		t.Fatal("blank model reported capacity")
	}
	var nilPool *AccountPool
	if nilPool.HasAvailableAccountForModel("claude-opus-5") {
		t.Fatal("nil pool reported capacity")
	}
}
