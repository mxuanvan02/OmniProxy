package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"omniproxy/config"
)

// withShortSVGTestBackoff shortens the real retry waits for the duration of one
// test. It changes timing only — the number of attempts and the classification
// decisions are unchanged, so a test still exercises the same control flow.
func withShortSVGTestBackoff(t *testing.T) {
	t.Helper()
	saved := svgTestRetryBackoff
	svgTestRetryBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { svgTestRetryBackoff = saved })
}

// TestSVGTestRetryable uses the message shapes the upstream builders actually
// produce (external_openai_responses.go, responses_upstream_parse.go,
// external_anthropic.go and friends), not invented text: a classifier that only
// matches strings no gateway emits would pass here and do nothing in the field.
func TestSVGTestRetryable(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		// The flakiness this retry exists for: api.apiforcode.com returned 400 for
		// a byte-identical think payload that succeeded on the next attempt,
		// because it round-robins across backends that disagree on request shape.
		{"gateway rejected a request its sibling accepts", `HTTP 400 from api.apiforcode.com: {"error":{"message":"Bad Request"}}`, true},
		{"gateway overload", `HTTP 429 from api.apiforcode.com: {"error":{"message":"Too Many Requests"}}`, true},
		{"bad gateway", `HTTP 502 from api.apiforcode.com: <html>Bad Gateway</html>`, true},
		{"unavailable", `HTTP 503 from api.apiforcode.com: upstream unavailable`, true},
		{"gateway timeout", `HTTP 504 from api.apiforcode.com: timeout`, true},
		{"server error", `HTTP 500 from api.apiforcode.com: internal error`, true},
		{"overloaded", `HTTP 529 from api.apiforcode.com: overloaded_error`, true},

		// The truncated-SSE path observed on the same gateway: an error frame with
		// an empty type falls through the parser switch and surfaces as this.
		{"stream ended before completion", `SSE stream ended before response.completed`, true},
		{"anthropic stream ended early", `external anthropic SSE stream ended before message_stop`, true},
		{"connection reset mid-request", `Post "https://api.apiforcode.com/v1/chat": read tcp: connection reset by peer`, true},
		{"unexpected eof", `unexpected EOF`, true},
		{"dial failure", `dial tcp 104.21.5.9:443: i/o timeout`, true},

		// Terminal: another attempt against the same account fails identically, so
		// retrying only costs the operator time and quota.
		{"model access denied", `HTTP 403 from api.apiforcode.com: {"message":"This token has no access to model qwen3.8-max"}`, false},
		{"unauthorised", `HTTP 401 from api.apiforcode.com: invalid api key`, false},
		{"unknown model", `HTTP 404 from api.apiforcode.com: model not found`, false},
		{"payload too large", `HTTP 413 from api.apiforcode.com: request entity too large`, false},
		{"malformed markup rejected", `HTTP 422 from api.apiforcode.com: unprocessable`, false},
		{"quota exhausted beats the retryable status", `HTTP 429 from api.apiforcode.com: {"error":{"message":"Quota exceeded for this account"}}`, false},
		{"billing", `HTTP 402 from api.apiforcode.com: insufficient balance`, false},
		{"caller cancelled", `context canceled`, false},
		{"deadline already spent", `context deadline exceeded`, false},

		// The gateway never sent response headers. Retrying this is the expensive
		// case: the client already burned ResponseHeaderTimeout (300s) once, so two
		// more attempts would make one failed rung take fifteen minutes. All four
		// attempts against api.hcnsec.cn landed within 360ms of each other, and a
		// reasoning-off raw probe failed identically, so the model was not slow —
		// the gateway was wedged. CallExternalOpenAI always asks for a stream, so a
		// responsive gateway would have sent headers in seconds.
		{"response header timeout", `external call KIMIK3: Post "https://api.hcnsec.cn/v1/chat/completions": http2: timeout awaiting response headers`, false},

		// No status and no transport signal: unknown, so leave it terminal rather
		// than spend attempts on a failure we cannot classify.
		{"unrecognised", `something else entirely`, false},
		{"empty message", ``, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := svgTestRetryable(errors.New(tt.msg)); got != tt.want {
				t.Errorf("svgTestRetryable(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
	if svgTestRetryable(nil) {
		t.Error("svgTestRetryable(nil) = true, want false")
	}
}

func TestSVGTestHTTPStatusCode(t *testing.T) {
	tests := []struct {
		msg  string
		want int
	}{
		{`HTTP 400 from host: body`, 400},
		{`HTTP 502 from AgentRouter (x@y.z): body`, 502},
		{`nested: HTTP 429 from host: {"detail":"HTTP 500 upstream"}`, 429},
		{`SSE stream ended before response.completed`, 0},
		{`connection refused`, 0},
	}
	for _, tt := range tests {
		if got := svgTestHTTPStatusCode(errors.New(tt.msg)); got != tt.want {
			t.Errorf("svgTestHTTPStatusCode(%q) = %d, want %d", tt.msg, got, tt.want)
		}
	}
}

// stubSVGTestDispatcher fails a fixed number of times then succeeds, recording
// how many attempts were made and optionally emitting output on a chosen attempt.
type stubSVGTestDispatcher struct {
	failures     []error
	emitOn       int
	attempts     int
	emitted      bool
	resetSeen    int
	failFastOnce bool
}

func (s *stubSVGTestDispatcher) dispatch(_ context.Context, _ *config.Account, _ *KiroPayload, callback *KiroStreamCallback) error {
	s.attempts++
	if s.emitOn == s.attempts && callback != nil && callback.OnOutput != nil {
		callback.OnOutput()
		s.emitted = true
	}
	if idx := s.attempts - 1; idx < len(s.failures) {
		if callback != nil && callback.OnReset != nil {
			s.resetSeen++
		}
		return s.failures[idx]
	}
	return nil
}

func newSVGTestCallback(emitted *bool) *KiroStreamCallback {
	produced := false
	return &KiroStreamCallback{
		OnOutput:  func() { produced = true; *emitted = true },
		HasOutput: func() bool { return produced },
		OnReset:   func() { produced = false; *emitted = false },
	}
}

func TestDispatchSVGTestWithRetryRetriesTransientFailures(t *testing.T) {
	withShortSVGTestBackoff(t)

	transient := errors.New(`HTTP 400 from api.apiforcode.com: Bad Request`)
	stub := &stubSVGTestDispatcher{failures: []error{transient, transient}}
	var emitted bool

	err := dispatchSVGTestWithRetry(context.Background(), &config.Account{ID: "a"}, pinnedPayload(), newSVGTestCallback(&emitted), stub.dispatch, svgTestMaxAttempts)
	if err != nil {
		t.Fatalf("expected success after two transient failures, got %v", err)
	}
	if stub.attempts != 3 {
		t.Errorf("attempts = %d, want 3 (one try plus two retries)", stub.attempts)
	}
}

// Exhausting the budget must surface the upstream error unchanged: the operator
// needs to see what the gateway actually said, not a generic "failed".
func TestDispatchSVGTestWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	withShortSVGTestBackoff(t)

	transient := errors.New(`HTTP 503 from api.apiforcode.com: unavailable`)
	stub := &stubSVGTestDispatcher{failures: []error{transient, transient, transient, transient}}
	var emitted bool

	err := dispatchSVGTestWithRetry(context.Background(), &config.Account{ID: "a"}, pinnedPayload(), newSVGTestCallback(&emitted), stub.dispatch, svgTestMaxAttempts)
	if err == nil {
		t.Fatal("expected the upstream error after exhausting retries, got nil")
	}
	if err.Error() != transient.Error() {
		t.Errorf("error = %q, want the upstream message %q", err, transient)
	}
	if stub.attempts != svgTestMaxAttempts {
		t.Errorf("attempts = %d, want %d", stub.attempts, svgTestMaxAttempts)
	}
}

// A terminal error must not consume the retry budget — 403 is a fact about the
// account, and retrying it only delays the answer.
func TestDispatchSVGTestWithRetryDoesNotRetryTerminalErrors(t *testing.T) {
	withShortSVGTestBackoff(t)

	stub := &stubSVGTestDispatcher{failures: []error{
		errors.New(`HTTP 403 from api.apiforcode.com: This token has no access to model qwen3.8-max`),
	}}
	var emitted bool

	err := dispatchSVGTestWithRetry(context.Background(), &config.Account{ID: "a"}, pinnedPayload(), newSVGTestCallback(&emitted), stub.dispatch, svgTestMaxAttempts)
	if err == nil {
		t.Fatal("expected the 403 to be returned, got nil")
	}
	if stub.attempts != 1 {
		t.Errorf("attempts = %d, want 1: a terminal error must not be retried", stub.attempts)
	}
}

// The safety property that keeps the stored SVG honest: once bytes have reached
// the callback, a retry would duplicate or discard partial output, so the loop
// must hand the error back instead of trying again.
func TestDispatchSVGTestWithRetryStopsOnceOutputEmitted(t *testing.T) {
	withShortSVGTestBackoff(t)

	stub := &stubSVGTestDispatcher{
		failures: []error{errors.New(`SSE stream ended before response.completed`), errors.New(`HTTP 503 from host: x`)},
		emitOn:   1,
	}
	var emitted bool

	err := dispatchSVGTestWithRetry(context.Background(), &config.Account{ID: "a"}, pinnedPayload(), newSVGTestCallback(&emitted), stub.dispatch, svgTestMaxAttempts)
	if err == nil {
		t.Fatal("expected an error when output was already emitted, got nil")
	}
	if stub.attempts != 1 {
		t.Errorf("attempts = %d, want 1: retrying after partial output would corrupt the reply", stub.attempts)
	}
	if !stub.emitted {
		t.Error("the stub never recorded emitted output, the test did not exercise the guard")
	}
}

// Between attempts the callback's accumulators are cleared through OnReset, so a
// retried run cannot concatenate the previous attempt's partial text into the
// stored SVG.
func TestDispatchSVGTestWithRetryResetsBetweenAttempts(t *testing.T) {
	withShortSVGTestBackoff(t)

	transient := errors.New(`HTTP 400 from host: Bad Request`)
	stub := &stubSVGTestDispatcher{failures: []error{transient, transient}}
	var emitted bool

	if err := dispatchSVGTestWithRetry(context.Background(), &config.Account{ID: "a"}, pinnedPayload(), newSVGTestCallback(&emitted), stub.dispatch, svgTestMaxAttempts); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if stub.resetSeen != 2 {
		t.Errorf("OnReset observed %d times, want 2 (once before each retry)", stub.resetSeen)
	}
}

// A cancelled context must stop the loop immediately rather than sit through a
// backoff, and the upstream error stays the reported reason for the failure.
func TestDispatchSVGTestWithRetryStopsOnCancelledContext(t *testing.T) {
	withShortSVGTestBackoff(t)

	ctx, cancel := context.WithCancel(context.Background())
	transient := errors.New(`HTTP 503 from host: unavailable`)
	stub := &stubSVGTestDispatcher{failures: []error{transient, transient, transient}}
	var emitted bool
	callback := newSVGTestCallback(&emitted)

	// Cancelling inside the first attempt is what the real gateway timeout does:
	// the shared context expires mid-call, so the backoff before attempt two must
	// notice and stop rather than wait out a sleep nobody will use.
	wrapped := &cancellingDispatcher{inner: stub.dispatch, cancel: cancel}

	err := dispatchSVGTestWithRetry(ctx, &config.Account{ID: "a"}, pinnedPayload(), callback, wrapped.dispatch, svgTestMaxAttempts)
	if err == nil {
		t.Fatal("expected the upstream error, got nil")
	}
	if !errors.Is(err, transient) {
		t.Errorf("error = %q, want the upstream message %q", err, transient)
	}
	if stub.attempts != 1 {
		t.Errorf("attempts = %d, want 1: a cancelled context must not be retried through", stub.attempts)
	}
}

// cancellingDispatcher cancels the context on its first call so the backoff
// before attempt two sees an already-done context.
type cancellingDispatcher struct {
	inner  svgTestDispatcher
	cancel context.CancelFunc
}

func (c *cancellingDispatcher) dispatch(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	err := c.inner(ctx, account, payload, callback)
	c.cancel()
	return err
}

// Attempt counts below one must still run exactly once: a misconfigured budget
// should degrade to the old single-dispatch behaviour, not skip the call.
func TestDispatchSVGTestWithRetryAttemptsAreAtLeastOne(t *testing.T) {
	withShortSVGTestBackoff(t)

	for _, attempts := range []int{0, -1} {
		stub := &stubSVGTestDispatcher{}
		var emitted bool
		if err := dispatchSVGTestWithRetry(context.Background(), &config.Account{ID: "a"}, pinnedPayload(), newSVGTestCallback(&emitted), stub.dispatch, attempts); err != nil {
			t.Fatalf("attempts=%d: %v", attempts, err)
		}
		if stub.attempts != 1 {
			t.Errorf("attempts=%d produced %d dispatches, want exactly 1", attempts, stub.attempts)
		}
	}
}

func TestSVGTestBackoffUsesTheConfiguredLadder(t *testing.T) {
	saved := svgTestRetryBackoff
	svgTestRetryBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { svgTestRetryBackoff = saved })

	ctx := context.Background()
	for retry, minWait := range []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond} {
		start := time.Now()
		if !svgTestBackoff(ctx, retry) {
			t.Fatalf("svgTestBackoff(ctx, %d) = false, want true", retry)
		}
		if elapsed := time.Since(start); retry > 0 && elapsed < minWait {
			t.Errorf("svgTestBackoff(ctx, %d) waited %v, want at least %v", retry, elapsed, minWait)
		}
	}
	// A retry index past the ladder must reuse the last rung, not panic.
	if !svgTestBackoff(ctx, 99) {
		t.Error("svgTestBackoff(ctx, 99) = false, want true")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if svgTestBackoff(cancelled, 1) {
		t.Error("svgTestBackoff on a cancelled context = true, want false")
	}
}
