package proxy

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"omniproxy/config"
)

// sseIdleWatchdog kills the upstream connection when no SSE ``data:`` line is
// received within the configured idle window. It exists because a byte-level
// idle reader (idleTimeoutReader) is defeated by SSE keepalive comments
// (``:keepalive\n``) that upstreams like ChatGPT/Codex emit periodically while
// thinking — those bytes reset the byte-level timer without carrying any
// actual response payload, so the client hangs indefinitely on a "200 OK but
// silent" upstream.
//
// The watchdog runs a goroutine that owns a timer. The SSE parse loop calls
// ``DataReceived()`` every time it processes a real ``data:`` line, which
// resets the timer. If the timer fires first, the watchdog closes the
// underlying body — this unblocks the pending ``bufio.Reader.ReadString`` call
// in the parse loop, which then surfaces ``ErrStreamIdleTimeout`` via
// ``TimedOut()``.
//
// Lifecycle: call ``Start()`` before the parse loop, ``DataReceived()`` on
// each ``data:`` line, ``Stop()`` when the parse loop exits (success or
// error), and ``TimedOut()`` after a read error to distinguish idle-timeout
// from a normal EOF / network error.
type sseIdleWatchdog struct {
	body     io.ReadCloser
	idle     time.Duration
	dataCh   chan struct{}
	doneCh   chan struct{}
	timedOut atomic.Bool
	startOnce sync.Once
	stopOnce sync.Once
}

// newSSEIdleWatchdog returns a watchdog for the given body, or nil when the
// idle timeout is disabled (config returns 0).
func newSSEIdleWatchdog(body io.ReadCloser) *sseIdleWatchdog {
	idle := config.GetStreamIdleTimeout()
	if idle <= 0 || body == nil {
		return nil
	}
	return &sseIdleWatchdog{
		body:   body,
		idle:   idle,
		dataCh: make(chan struct{}, 1),
		doneCh: make(chan struct{}),
	}
}

// Start launches the watchdog goroutine. Safe to call once.
func (w *sseIdleWatchdog) Start() {
	w.startOnce.Do(func() {
		go w.run()
	})
}

func (w *sseIdleWatchdog) run() {
	timer := time.NewTimer(w.idle)
	defer timer.Stop()
	for {
		select {
		case <-w.doneCh:
			return
		case <-w.dataCh:
			timer.Reset(w.idle)
		case <-timer.C:
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

// DataReceived signals that a real ``data:`` line was processed. Non-blocking
// so the parse loop is never stalled by a full channel.
func (w *sseIdleWatchdog) DataReceived() {
	select {
	case w.dataCh <- struct{}{}:
	default:
	}
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
