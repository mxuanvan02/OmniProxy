package proxy

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestSSEIdleWatchdogKillsOnNoData verifies the watchdog closes the body and
// sets TimedOut() when no ``data:`` line arrives within the idle window.
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
		dataCh: make(chan struct{}, 1),
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

// TestSSEIdleWatchdogResetsOnData verifies the watchdog does NOT fire when
// data: lines arrive within the idle window.
func TestSSEIdleWatchdogResetsOnData(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"text\":\"hello\"}\n"))
	w := &sseIdleWatchdog{
		body:   body,
		idle:   100 * time.Millisecond,
		dataCh: make(chan struct{}, 1),
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
}

// keepaliveOnlyReader emits ``:keepalive\n`` lines on a timer but never
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
