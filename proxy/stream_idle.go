package proxy

import (
	"errors"
	"io"
	"time"

	"omniproxy/config"
)

// ErrStreamIdleTimeout is returned when an upstream streaming response has
// not produced any byte within the configured idle window (see
// config.GetStreamIdleTimeout). It is intentionally a sentinel so the
// failover layer (isNetworkError / handleAccountFailure) can match it and
// rotate to another account instead of waiting for the overall HTTP
// client timeout (which defaults to 5 minutes).
//
// The error string is matched by isNetworkError in account_failover.go via
// the "stream idle timeout" substring — keep that phrase stable if you
// refactor the message.
var ErrStreamIdleTimeout = errors.New("stream idle timeout: upstream produced no data within idle window")

// idleTimeoutReader wraps an upstream response body and aborts the read
// when no byte arrives within the idle window. It serves two purposes:
//
//  1. TTFB deadline — the very first Read must produce data within the
//     idle window, otherwise the upstream is treated as a "200 OK but
//     silent" hang and the connection is killed.
//  2. Per-chunk idle deadline — every subsequent Read resets the same
//     window, so a stream that stalls mid-way (e.g. account rate-limited
//     after the first event) is also caught.
//
// The reader is a no-op when config.GetStreamIdleTimeout() returns 0; in
// that case it delegates directly to the underlying body and the overall
// http.Client.Timeout (KiroApiTimeout, default 5m) governs the request.
//
// The wrapped body's Close is propagated so callers can `defer resp.Body.Close()`
// as usual; on timeout the body is also closed to release the connection
// back to the pool (otherwise the stalled socket lingers until the
// transport's idle timeout reaps it).
type idleTimeoutReader struct {
	body       io.ReadCloser
	idle       time.Duration
	timer      *time.Timer
	done       chan struct{} // closed by the timer goroutine on fire
	readCh     chan readResult
	closeCh    chan struct{}
	closed     bool
}

type readResult struct {
	n   int
	err error
}

// newIdleTimeoutReader wraps body with an idle deadline. Returns body
// unchanged when the idle timeout is disabled (zero).
func newIdleTimeoutReader(body io.ReadCloser) io.ReadCloser {
	idle := config.GetStreamIdleTimeout()
	if idle <= 0 || body == nil {
		return body
	}
	return &idleTimeoutReader{
		body:    body,
		idle:    idle,
		readCh:  make(chan readResult, 1),
		closeCh: make(chan struct{}),
	}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.EOF
	}

	// Reset the timer for this Read. We allocate a fresh timer per Read
	// rather than reusing one across reads — simpler than juggling Reset
	// races, and Read calls are not on the hottest path (one per SSE
	// chunk, which is at most a few hundred per request).
	timer := time.NewTimer(r.idle)
	defer timer.Stop()

	// Kick off the actual read in a goroutine so we can race it against
	// the timer. The goroutine is bounded: it returns as soon as body.Read
	// returns (which it will, either with data or with the error from
	// Close below when the reader is shut down).
	go func() {
		n, err := r.body.Read(p)
		select {
		case r.readCh <- readResult{n: n, err: err}:
		case <-r.closeCh:
		}
	}()

	select {
	case res := <-r.readCh:
		return res.n, res.err
	case <-timer.C:
		// Idle window elapsed — kill the connection and surface a sentinel
		// error so the failover layer rotates to another account.
		r.kill()
		return 0, ErrStreamIdleTimeout
	}
}

func (r *idleTimeoutReader) Close() error {
	r.kill()
	return r.body.Close()
}

// kill signals the read goroutine to stop waiting and marks the reader
// closed so subsequent Reads short-circuit to io.EOF. It does NOT close
// the underlying body — Close() does that, and kill() may be called from
// Read (on timeout) where the caller still owns the body lifecycle via
// defer.
func (r *idleTimeoutReader) kill() {
	if r.closed {
		return
	}
	r.closed = true
	close(r.closeCh)
}
