package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCheckAndKillExistingNoPIDFile: a missing lock file must report "nothing
// running" rather than refusing to start.
func TestCheckAndKillExistingNoPIDFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "omniproxy.pid")
	if CheckAndKillExisting(pidPath) {
		t.Fatal("missing PID file must report no existing process")
	}
}

// TestCheckAndKillExistingRemovesGarbagePIDFile: a truncated or corrupted lock
// file (crash mid-write, filled disk) must be cleaned up, not treated as a live
// process — otherwise the binary refuses to start forever.
func TestCheckAndKillExistingRemovesGarbagePIDFile(t *testing.T) {
	for _, content := range []string{"", "not-a-pid", "12x34", "\n\n"} {
		pidPath := filepath.Join(t.TempDir(), "omniproxy.pid")
		if err := os.WriteFile(pidPath, []byte(content), 0644); err != nil {
			t.Fatalf("seed pid file: %v", err)
		}
		if CheckAndKillExisting(pidPath) {
			t.Fatalf("garbage PID %q reported as a live process", content)
		}
		if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
			t.Fatalf("garbage PID file %q was not removed (stat err=%v)", content, err)
		}
	}
}

// TestCheckAndKillExistingRemovesStalePIDFile is the stale-lock false-positive
// guard: a PID belonging to no live process must be cleaned up and reported as
// not running. A dead PID recorded as alive is a silent refuse-to-start.
func TestCheckAndKillExistingRemovesStalePIDFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "omniproxy.pid")

	// Reap a real child so its PID is genuinely dead (no zombie, which would
	// still answer signal 0 on this platform).
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn throwaway process: %v", err)
	}
	deadPID := cmd.Process.Pid
	if platformProcessAlive(deadPID) {
		t.Skipf("PID %d recycled or still reported alive; cannot test stale detection", deadPID)
	}

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)+"\n"), 0644); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}
	if CheckAndKillExisting(pidPath) {
		t.Fatalf("stale PID %d reported as a live process", deadPID)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("stale PID file not removed (stat err=%v)", err)
	}
}

// TestCheckAndKillExistingTerminatesLiveProcess covers the positive branch: a
// PID that is actually alive must be signalled, waited on, and the lock removed.
// Without this half the stale-lock tests above would pass on a function that
// always returns false.
func TestCheckAndKillExistingTerminatesLiveProcess(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "omniproxy.pid")

	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatalf("start victim: %v", err)
	}
	pid := victim.Process.Pid
	defer func() {
		_ = victim.Process.Kill()
		_, _ = victim.Process.Wait()
	}()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}
	if !CheckAndKillExisting(pidPath) {
		t.Fatalf("live PID %d not reported as an existing process", pid)
	}
	// Reap before probing liveness: an unreaped child stays signal-addressable
	// as a zombie, which would make the assertion below meaningless.
	_, _ = victim.Process.Wait()
	if platformProcessAlive(pid) {
		t.Fatalf("PID %d still alive after CheckAndKillExisting", pid)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file not removed after kill (stat err=%v)", err)
	}
}

// TestWritePIDCreatesParentDirAndRecordsOwnPID: the lock must be creatable on a
// fresh install where data/ does not exist yet.
func TestWritePIDCreatesParentDirAndRecordsOwnPID(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "nested", "data", "omniproxy.pid")
	if err := WritePID(pidPath); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid file content %q not numeric: %v", data, err)
	}
	if got != os.Getpid() {
		t.Fatalf("recorded PID %d, want own PID %d", got, os.Getpid())
	}
}

// TestRemovePIDOnlyRemovesOwnLock is the cross-instance-safety guard: a second
// instance must never delete the lock held by a different live process, or two
// servers end up bound to the same port with no lock at all.
func TestRemovePIDOnlyRemovesOwnLock(t *testing.T) {
	dir := t.TempDir()

	own := filepath.Join(dir, "own.pid")
	if err := WritePID(own); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	RemovePID(own)
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("own PID file not removed (stat err=%v)", err)
	}

	foreign := filepath.Join(dir, "foreign.pid")
	if err := os.WriteFile(foreign, []byte(strconv.Itoa(os.Getpid()+1)), 0644); err != nil {
		t.Fatalf("seed foreign pid: %v", err)
	}
	RemovePID(foreign)
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign PID file was removed: %v", err)
	}

	// Unparseable content is also somebody else's problem — RemovePID must not
	// delete it (CheckAndKillExisting owns garbage cleanup).
	garbage := filepath.Join(dir, "garbage.pid")
	if err := os.WriteFile(garbage, []byte("nonsense"), 0644); err != nil {
		t.Fatalf("seed garbage pid: %v", err)
	}
	RemovePID(garbage)
	if _, err := os.Stat(garbage); err != nil {
		t.Fatalf("garbage PID file removed by RemovePID: %v", err)
	}

	RemovePID(filepath.Join(dir, "absent.pid")) // must not panic
}

// TestPIDPathIsAbsoluteAndNamedConsistently: both branches must yield an
// absolute path ending in the expected filename, so the lock cannot land in an
// unpredictable relative location.
func TestPIDPathIsAbsoluteAndNamedConsistently(t *testing.T) {
	got := PIDPath(filepath.Join(t.TempDir(), "config.json"))
	if filepath.Base(got) != "omniproxy.pid" {
		t.Fatalf("PIDPath = %q, want basename omniproxy.pid", got)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("PIDPath = %q, want an absolute path", got)
	}
}
