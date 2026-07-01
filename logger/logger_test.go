package logger

import (
	"strings"
	"sync"
	"testing"
)

// TestSubscribeReceivesNewLines verifies a subscriber is invoked with each
// formatted log line written after it subscribes, and that the unsubscribe
// function stops further delivery.
func TestSubscribeReceivesNewLines(t *testing.T) {
	SetLevel(LevelDebug)

	var mu sync.Mutex
	var got []string
	unsub := Subscribe(func(line string) {
		mu.Lock()
		got = append(got, line)
		mu.Unlock()
	})

	Infof("hello %d", 1)
	Errorf("boom %s", "x")

	mu.Lock()
	afterTwo := len(got)
	mu.Unlock()
	if afterTwo != 2 {
		t.Fatalf("expected 2 lines delivered, got %d: %v", afterTwo, got)
	}
	if !strings.Contains(got[0], "hello 1") || !strings.HasPrefix(got[0], "INFO  ") {
		t.Fatalf("first line wrong: %q", got[0])
	}
	if !strings.Contains(got[1], "boom x") || !strings.HasPrefix(got[1], "ERROR ") {
		t.Fatalf("second line wrong: %q", got[1])
	}

	// After unsubscribing, no further lines should arrive.
	unsub()
	Infof("after unsub")

	mu.Lock()
	final := len(got)
	mu.Unlock()
	if final != 2 {
		t.Fatalf("expected no delivery after unsubscribe, got %d lines", final)
	}
}

// TestSubscribeRespectsLevel verifies that a message filtered out by the active
// level is neither logged nor delivered to subscribers.
func TestSubscribeRespectsLevel(t *testing.T) {
	SetLevel(LevelWarn)
	defer SetLevel(LevelInfo)

	var mu sync.Mutex
	count := 0
	unsub := Subscribe(func(string) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	defer unsub()

	Infof("below threshold")  // dropped by level
	Warnf("at threshold")     // delivered

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("expected only the WARN line delivered, got %d", count)
	}
}
