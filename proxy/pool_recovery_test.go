package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// shortenPoolRecoveryDelay keeps the wave logic under test without sleeping for
// the production delay.
func shortenPoolRecoveryDelay(t *testing.T) {
	t.Helper()
	original := poolRecoveryDelay
	poolRecoveryDelay = 5 * time.Millisecond
	t.Cleanup(func() { poolRecoveryDelay = original })
}

// A burst of transient upstream errors can exclude every eligible account
// before the answer is finished, which the client renders as the assistant
// stopping mid-sentence. Recovery re-scans the pool a bounded number of times.
func TestWaitForPoolRecoveryRetriesTransientExhaustion(t *testing.T) {
	shortenPoolRecoveryDelay(t)

	h := &Handler{}
	excluded := map[string]bool{"acct-1": true, "acct-2": true}
	wave := 0
	err := errors.New("HTTP 503 from upstream: service unavailable")

	for i := 1; i <= poolRecoveryWaves; i++ {
		if !h.waitForPoolRecovery(context.Background(), "claude-opus-5", excluded, err, &wave) {
			t.Fatalf("wave %d: recovery refused a transient exhaustion", i)
		}
		if wave != i {
			t.Fatalf("wave counter = %d, want %d", wave, i)
		}
		if len(excluded) != 0 {
			t.Fatalf("wave %d left %d accounts excluded; the re-scan has nothing to select", i, len(excluded))
		}
		excluded["acct-1"] = true
	}

	// The budget is bounded: without this the request could loop on a pool that
	// is genuinely down.
	if h.waitForPoolRecovery(context.Background(), "claude-opus-5", excluded, err, &wave) {
		t.Fatalf("recovery exceeded its %d-wave budget", poolRecoveryWaves)
	}
}

func TestWaitForPoolRecoveryRefusesNonRetryableCases(t *testing.T) {
	shortenPoolRecoveryDelay(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		excluded map[string]bool
		err      error
		wave     int
	}{
		{
			name:     "auth failure is not transient",
			ctx:      context.Background(),
			excluded: map[string]bool{"acct-1": true},
			err:      errors.New("HTTP 401 from Kiro IDE: Authentication failed - token invalid or expired"),
		},
		{
			name:     "quota exhaustion must not be retried",
			ctx:      context.Background(),
			excluded: map[string]bool{"acct-1": true},
			err:      errors.New("HTTP 429: quota exhausted"),
		},
		{
			name:     "client hang-up is not an account fault",
			ctx:      cancelled,
			excluded: map[string]bool{"acct-1": true},
			err:      errors.New("HTTP 503 from upstream: service unavailable"),
		},
		{
			name:     "nothing was excluded, so the pool is simply empty",
			ctx:      context.Background(),
			excluded: map[string]bool{},
			err:      errors.New("HTTP 503 from upstream: service unavailable"),
		},
		{
			name:     "no error means the pool never had a candidate",
			ctx:      context.Background(),
			excluded: map[string]bool{"acct-1": true},
			err:      nil,
		},
		{
			name:     "budget already spent",
			ctx:      context.Background(),
			excluded: map[string]bool{"acct-1": true},
			err:      errors.New("HTTP 503 from upstream: service unavailable"),
			wave:     poolRecoveryWaves,
		},
	}

	h := &Handler{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wave := tc.wave
			before := len(tc.excluded)
			if h.waitForPoolRecovery(tc.ctx, "claude-opus-5", tc.excluded, tc.err, &wave) {
				t.Fatal("recovery retried a case it must fail through")
			}
			if wave != tc.wave {
				t.Fatalf("wave counter moved to %d without a retry", wave)
			}
			if len(tc.excluded) != before {
				t.Fatalf("exclusion set was cleared without a retry: %d → %d", before, len(tc.excluded))
			}
		})
	}
}

// Every account-selection loop needs the recovery hook. A path that lacks it
// ends the turn on the first transient burst while its sibling path recovers,
// which reads as the same model failing only on some endpoints.
func TestAllAccountLoopsUsePoolRecovery(t *testing.T) {
	for _, path := range []struct {
		file   string
		marker string
	}{
		{file: "handler.go", marker: "claude-stream"},
		{file: "handler.go", marker: "claude-nonstream"},
		{file: "handler.go", marker: "openai-stream"},
		{file: "handler.go", marker: "openai-nonstream"},
		{file: "responses_handler.go", marker: "responses-stream"},
		{file: "responses_handler.go", marker: "responses-nonstream"},
	} {
		if !accountLoopHasRecovery(t, path.file, path.marker) {
			t.Errorf("%s: the %q selection loop has no pool-recovery wave", path.file, path.marker)
		}
	}
}

// accountLoopHasRecovery reports whether the selection loop identified by its
// logCacheRouting marker calls waitForPoolRecovery. The check reads the source
// because the loops are inside long unexported handlers that need a live
// upstream, an account pool and a ResponseWriter to reach — a structural
// assertion catches the omission that a behavioural test of one path cannot.
func accountLoopHasRecovery(t *testing.T, file, marker string) bool {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := strings.Split(string(src), "\n")
	at := -1
	for i, line := range lines {
		if strings.Contains(line, `logCacheRouting("`+marker+`"`) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("%s: no selection loop marked %q", file, marker)
	}
	// The recovery hook sits in the `account == nil` branch, a handful of lines
	// above the marker. Walking back to the loop header keeps the window tight
	// enough that a neighbouring loop's hook cannot be mistaken for this one.
	for i := at; i >= 0 && i > at-15; i-- {
		if strings.Contains(lines[i], "waitForPoolRecovery(") {
			return true
		}
		if strings.Contains(lines[i], "for attempt := 0") && i != at {
			return false
		}
	}
	return false
}
