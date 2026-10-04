package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInitCancelsOrphanedCoalescedSave is the regression test for the CI flake
// "TempDir RemoveAll cleanup: directory not empty" (seen on
// TestCallExternalOpenAIJSONFallback under the race detector).
//
// Mechanism: scheduleSave hands a token to the process-global saveLoop
// goroutine, which sleeps coalescedSaveDelay before writing cfgPath. When a
// test ends and the next one calls Init with a fresh t.TempDir(), the sleeper
// wakes mid-test and writes a temp file into the NEW directory — and if that
// lands during the new test's TempDir cleanup, removal fails with "directory
// not empty". The victim test is not the one that scheduled the save.
//
// The fix under test: Init drains the queued token and clears pendingSave
// (flushing outstanding counters to the OLD path first), so the sleeper's
// FlushPendingSave is a no-op when it wakes.
func TestInitCancelsOrphanedCoalescedSave(t *testing.T) {
	dirA := t.TempDir()
	pathA := filepath.Join(dirA, "config.json")
	if err := Init(pathA); err != nil {
		t.Fatalf("Init(A): %v", err)
	}

	// Schedule a coalesced save, as UpdateAccountStats does on every chat turn.
	// saveLoop receives the token almost immediately and starts its 2s sleep.
	scheduleSave()
	if !pendingSave.Load() {
		t.Fatal("scheduleSave did not mark a pending save")
	}

	// Repoint to a new directory — what the next test's Init(t.TempDir()) does.
	dirB := t.TempDir()
	pathB := filepath.Join(dirB, "config.json")
	if err := Init(pathB); err != nil {
		t.Fatalf("Init(B): %v", err)
	}

	// The orphaned save must be cancelled, not merely deferred.
	if pendingSave.Load() {
		t.Error("pendingSave still set after Init(B): the orphaned coalesced save was not cancelled")
	}

	// The outstanding counters are persisted to the OLD path, not dropped.
	if _, err := os.Stat(pathA); err != nil {
		t.Errorf("config.json not flushed to old path on re-Init: %v", err)
	}

	// Snapshot dirB right after Init: a first-run Init legitimately writes its
	// own baseline config.json (password generation etc.), which is NOT the
	// orphaned save. The orphan shows up as files appearing AFTER the sleep —
	// on the unfixed code saveLoop wakes and rewrites pathB via a
	// .config.json.tmp-* sibling, exactly what made an unrelated test's
	// TempDir cleanup fail with "directory not empty".
	before := map[string]bool{}
	if entries, err := os.ReadDir(dirB); err == nil {
		for _, e := range entries {
			before[e.Name()] = true
		}
	}
	var modBefore time.Time
	if fi, err := os.Stat(pathB); err == nil {
		modBefore = fi.ModTime()
	}

	// Wait past the sleeper's delay.
	time.Sleep(coalescedSaveDelay + 500*time.Millisecond)

	entries, err := os.ReadDir(dirB)
	if err != nil {
		t.Fatalf("ReadDir(B): %v", err)
	}
	for _, e := range entries {
		if !before[e.Name()] {
			t.Errorf("orphaned coalesced save wrote into the new config dir: %s", e.Name())
		}
	}
	// Even a rewrite of the baseline file counts: its mtime must not move.
	if fi, err := os.Stat(pathB); err == nil && fi.ModTime().After(modBefore) {
		t.Error("pathB rewritten during the sleep window")
	}
}
