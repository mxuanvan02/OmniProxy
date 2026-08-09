package proxy

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestSSEIdleWatchdogKillsOnNoData verifies the watchdog closes the body and
// sets TimedOut() when no `data:` line arrives within the idle window.
// This simulates a "200 OK but silent" upstream that only emits keepalive
// comments — the exact bug that the byte-level idleTimeoutReader cannot
// catch.
func TestSSEIdleWatchdogKillsOnNoData(t *testing.T) {
	// Build a body that emits a keepalive comment every 20ms but never a
	// data: line. The watchdog must fire after the idle window regardless
	// of the keepalive bytes.
	body := &keepaliveOnlyReader{interval: 20 * time.Millisecond}
	w := &sseIdleWatchdog{
		body:   body,
		idle:   80 * time.Millisecond,
		doneCh: make(chan struct{}),
	}
	w.Start()
	defer w.Stop()

	// Simulate the parse loop: read lines, never call DataReceived.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("watchdog did not fire within 2s")
		default:
		}
		if w.TimedOut() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !w.TimedOut() {
		t.Fatal("expected TimedOut() == true")
	}
}

func TestSSEIdleWatchdogUsesShorterInitialDataDeadline(t *testing.T) {
	body := &keepaliveOnlyReader{interval: 10 * time.Millisecond}
	w := &sseIdleWatchdog{
		body:        body,
		idle:        time.Second,
		initialIdle: 60 * time.Millisecond,
		doneCh:      make(chan struct{}),
	}
	w.Start()
	defer w.Stop()

	deadline := time.Now().Add(300 * time.Millisecond)
	for !w.TimedOut() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !w.TimedOut() {
		t.Fatal("watchdog did not enforce the initial-data deadline")
	}
}

// TestSSEIdleWatchdogResetsOnData verifies the watchdog does NOT fire when
// data: lines arrive within the idle window, then fires after data stops.
func TestSSEIdleWatchdogResetsOnData(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"text\":\"hello\"}\n"))
	w := &sseIdleWatchdog{
		body:   body,
		idle:   100 * time.Millisecond,
		doneCh: make(chan struct{}),
	}
	w.Start()
	defer w.Stop()

	// Call DataReceived every 50ms — faster than the 100ms idle window.
	for i := 0; i < 5; i++ {
		w.DataReceived()
		time.Sleep(50 * time.Millisecond)
		if w.TimedOut() {
			t.Fatal("watchdog fired despite regular DataReceived calls")
		}
	}

	// Once data stops, the same watchdog must still enforce the idle limit.
	deadline := time.Now().Add(500 * time.Millisecond)
	for !w.TimedOut() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.TimedOut() {
		t.Fatal("watchdog did not fire after data stopped")
	}
}

// keepaliveOnlyReader emits `:keepalive\n` lines on a timer but never
// returns a data: line. It blocks on Read until the next keepalive is due.
type keepaliveOnlyReader struct {
	interval time.Duration
	closed   bool
}

func (k *keepaliveOnlyReader) Read(p []byte) (int, error) {
	if k.closed {
		return 0, io.ErrClosedPipe
	}
	time.Sleep(k.interval)
	line := ":keepalive\n"
	n := copy(p, []byte(line))
	return n, nil
}

func (k *keepaliveOnlyReader) Close() error {
	k.closed = true
	return nil
}
