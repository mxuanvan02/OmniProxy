package proxy

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestIdleTimeoutReaderTTFB verifies the reader aborts when the upstream
// never sends the first byte within the idle window.
func TestIdleTimeoutReaderTTFB(t *testing.T) {
	// Force a 100ms idle window for the test.
	t.Setenv("STREAM_IDLE_TIMEOUT_SECONDS", "0") // disable env override so we exercise the struct directly

	slow := &neverReader{}
	r := &idleTimeoutReader{
		body:    slow,
		idle:    50 * time.Millisecond,
		readCh:  make(chan readResult, 1),
		closeCh: make(chan struct{}),
	}

	start := time.Now()
	_, err := r.Read(make([]byte, 16))
	elapsed := time.Since(start)

	if err != ErrStreamIdleTimeout {
		t.Fatalf("expected ErrStreamIdleTimeout, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("idle reader did not abort fast enough: %v", elapsed)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("idle reader aborted too fast (false positive): %v", elapsed)
	}
}

// TestIdleTimeoutReaderPassesData verifies the reader is transparent when
// the upstream produces data within the idle window.
func TestIdleTimeoutReaderPassesData(t *testing.T) {
	r := &idleTimeoutReader{
		body:    io.NopCloser(strings.NewReader("hello world")),
		idle:    5 * time.Second,
		readCh:  make(chan readResult, 1),
		closeCh: make(chan struct{}),
	}
	buf := make([]byte, 32)
	n, err := r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(buf[:n]))
	}
}

// TestIdleTimeoutReaderDisabledWhenZero verifies that
// newIdleTimeoutReader returns the body unchanged when the idle timeout
// is disabled (env=0).
func TestIdleTimeoutReaderDisabledWhenZero(t *testing.T) {
	t.Setenv("STREAM_IDLE_TIMEOUT_SECONDS", "0")
	body := io.NopCloser(strings.NewReader("data"))
	got := newIdleTimeoutReader(body)
	if got != body {
		t.Fatalf("expected body unchanged when idle timeout disabled, got %T", got)
	}
}

// neverReader blocks forever on Read (simulates a "200 OK but silent"
// upstream account).
type neverReader struct{}

func (n *neverReader) Read(p []byte) (int, error) {
	time.Sleep(10 * time.Second)
	return 0, io.EOF
}

func (n *neverReader) Close() error { return nil }
