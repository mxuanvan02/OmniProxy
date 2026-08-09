package proxy

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"omniproxy/config"
)

// sseIdleWatchdog kills the upstream connection when no SSE `data:` line is
// received within the configured idle window. It exists because a byte-level
// idle reader (idleTimeoutReader) is defeated by SSE keepalive comments
// (`:keepalive\n`) that upstreams like ChatGPT/Codex emit periodically while
// thinking — those bytes reset the byte-level timer without carrying any
// actual response payload, so the client hangs indefinitely on a "200 OK but
// silent" upstream.
//
// DataReceived records a monotonic timestamp in an atomic integer. The
// watchdog goroutine wakes only when the current deadline expires (or Stop is
// called), then checks whether newer data extended that deadline. This avoids
// waking a second goroutine for every token in a high-throughput SSE stream.
// If the deadline really expired, the watchdog closes the underlying body to
// unblock the pending `bufio.Reader.ReadString` call; the parse loop then
// surfaces `ErrStreamIdleTimeout` via `TimedOut()`.
//
// Lifecycle: call `Start()` before the parse loop, `DataReceived()` on
// each `data:` line, `Stop()` when the parse loop exits (success or
// error), and `TimedOut()` after a read error to distinguish idle-timeout
// from a normal EOF / network error.
type sseIdleWatchdog struct {
	body          io.ReadCloser
	idle          time.Duration
	initialIdle   time.Duration
	doneCh        chan struct{}
	startedAt     time.Time
	lastDataNanos atomic.Int64
	timedOut      atomic.Bool
	startOnce     sync.Once
	stopOnce      sync.Once
}

// newSSEIdleWatchdog returns a watchdog for the given body, or nil when the
// idle timeout is disabled (config returns 0).
func newSSEIdleWatchdog(body io.ReadCloser) *sseIdleWatchdog {
	idle := config.GetStreamIdleTimeout()
	if idle <= 0 || body == nil {
		return nil
	}
	return &sseIdleWatchdog{
		body:        body,
		idle:        idle,
		initialIdle: boundedInitialStreamTimeout(idle),
		doneCh:      make(chan struct{}),
	}
}

// Start launches the watchdog goroutine. Safe to call once.
func (w *sseIdleWatchdog) Start() {
	w.startOnce.Do(func() {
		w.startedAt = time.Now()
		go w.run()
	})
}

func (w *sseIdleWatchdog) run() {
	initialIdle := w.initialIdle
	if initialIdle <= 0 || initialIdle > w.idle {
		initialIdle = w.idle
	}
	timer := time.NewTimer(initialIdle)
	defer timer.Stop()
	for {
		select {
		case <-w.doneCh:
			return
		case <-timer.C:
			lastData := time.Duration(w.lastDataNanos.Load())
			idle := w.idle
			if lastData == 0 {
				idle = initialIdle
			}
			remaining := idle - (time.Since(w.startedAt) - lastData)
			if remaining > 0 {
				timer.Reset(remaining)
				continue
			}

			// If data arrived while the deadline was being evaluated, honor
			// the newer timestamp instead of closing a healthy stream.
			if latest := time.Duration(w.lastDataNanos.Load()); latest != lastData {
				remaining = w.idle - (time.Since(w.startedAt) - latest)
				if remaining > 0 {
					timer.Reset(remaining)
					continue
				}
			}

			w.timedOut.Store(true)
			// Close the upstream body to unblock the pending ReadString in
			// the parse loop. The parse loop will get an error (typically
			// io.ErrClosedPipe or "read on closed reader") and check
			// TimedOut() to surface ErrStreamIdleTimeout.
			_ = w.body.Close()
			return
		}
	}
}

// DataReceived records that a real `data:` line was processed. It does not
// signal the watchdog goroutine, so high token rates do not create an extra
// scheduler wakeup per SSE event.
func (w *sseIdleWatchdog) DataReceived() {
	w.lastDataNanos.Store(time.Since(w.startedAt).Nanoseconds())
}

// Stop signals the watchdog goroutine to exit. Safe to call once.
func (w *sseIdleWatchdog) Stop() {
	w.stopOnce.Do(func() {
		close(w.doneCh)
	})
}

// TimedOut returns true if the watchdog fired and closed the body.
func (w *sseIdleWatchdog) TimedOut() bool {
	return w.timedOut.Load()
}
