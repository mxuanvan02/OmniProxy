package logger

import (
	"fmt"
	"sync"
	"testing"
)

// The SSE log viewer resumes with Last-Event-ID, so every buffered line needs a
// stable, monotonically increasing sequence number. These tests lock down the
// three properties the resume path depends on:
//
//  1. sequence numbers are contiguous and survive ring wraparound,
//  2. SeqLines(after, limit) returns only what the client is missing,
//  3. AppendLine reports the sequence it assigned, atomically.

func TestAppendLineReturnsMonotonicSeq(t *testing.T) {
	rb := NewRingBuffer(4)
	for i := 1; i <= 10; i++ {
		got := rb.AppendLine(fmt.Sprintf("line-%d", i))
		if got != uint64(i) {
			t.Fatalf("AppendLine #%d returned seq %d, want %d", i, got, i)
		}
	}
	if rb.Seq() != 10 {
		t.Fatalf("Seq() = %d, want 10", rb.Seq())
	}
}

func TestSeqLinesAfterCursor(t *testing.T) {
	rb := NewRingBuffer(10)
	for i := 1; i <= 5; i++ {
		rb.AppendLine(fmt.Sprintf("line-%d", i))
	}

	// after=0 is a fresh client: everything retained.
	all := rb.SeqLines(0, 0)
	if len(all) != 5 {
		t.Fatalf("SeqLines(0,0) len = %d, want 5", len(all))
	}
	if all[0].Seq != 1 || all[4].Seq != 5 {
		t.Fatalf("seq range = [%d..%d], want [1..5]", all[0].Seq, all[4].Seq)
	}

	// A resuming client that already has through seq 3 gets only 4 and 5.
	tail := rb.SeqLines(3, 0)
	if len(tail) != 2 {
		t.Fatalf("SeqLines(3,0) len = %d, want 2", len(tail))
	}
	if tail[0].Seq != 4 || tail[0].Line != "line-4" {
		t.Fatalf("first resumed = seq %d %q, want seq 4 \"line-4\"", tail[0].Seq, tail[0].Line)
	}

	// Fully caught up: nothing to send. This is the case that used to replay the
	// entire 2048-line buffer on every reconnect.
	if got := rb.SeqLines(5, 0); len(got) != 0 {
		t.Fatalf("SeqLines(5,0) len = %d, want 0", len(got))
	}

	// A cursor ahead of the buffer (should not happen, but must not panic or
	// return garbage).
	if got := rb.SeqLines(999, 0); len(got) != 0 {
		t.Fatalf("SeqLines(999,0) len = %d, want 0", len(got))
	}
}

func TestSeqLinesRespectsLimit(t *testing.T) {
	rb := NewRingBuffer(100)
	for i := 1; i <= 50; i++ {
		rb.AppendLine(fmt.Sprintf("line-%d", i))
	}

	// tail=10 must yield the *newest* 10, not the oldest.
	got := rb.SeqLines(0, 10)
	if len(got) != 10 {
		t.Fatalf("SeqLines(0,10) len = %d, want 10", len(got))
	}
	if got[0].Seq != 41 || got[9].Seq != 50 {
		t.Fatalf("limited range = [%d..%d], want [41..50]", got[0].Seq, got[9].Seq)
	}
}

func TestSeqLinesAfterWraparound(t *testing.T) {
	// Capacity 3, 7 writes: only seq 5,6,7 are still retained. A client resuming
	// from seq 2 has fallen off the back — resume is best-effort, so it should
	// get what remains rather than an error or a gap-free guarantee.
	rb := NewRingBuffer(3)
	for i := 1; i <= 7; i++ {
		rb.AppendLine(fmt.Sprintf("line-%d", i))
	}

	got := rb.SeqLines(0, 0)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (capacity)", len(got))
	}
	if got[0].Seq != 5 || got[2].Seq != 7 {
		t.Fatalf("retained range = [%d..%d], want [5..7]", got[0].Seq, got[2].Seq)
	}
	if got[0].Line != "line-5" {
		t.Fatalf("oldest retained line = %q, want \"line-5\"", got[0].Line)
	}

	stale := rb.SeqLines(2, 0)
	if len(stale) != 3 {
		t.Fatalf("stale cursor len = %d, want 3 (all retained)", len(stale))
	}
	if stale[0].Seq != 5 {
		t.Fatalf("stale cursor first seq = %d, want 5", stale[0].Seq)
	}
}

func TestSeqLinesEmptyBuffer(t *testing.T) {
	rb := NewRingBuffer(8)
	if got := rb.SeqLines(0, 0); got != nil && len(got) != 0 {
		t.Fatalf("empty buffer returned %d lines, want 0", len(got))
	}
}

func TestLinesStillChronologicalAfterSeqChange(t *testing.T) {
	// Lines() is used by the non-SSE /logs endpoint; the seq refactor must not
	// change its ordering contract.
	rb := NewRingBuffer(3)
	for i := 1; i <= 5; i++ {
		rb.AppendLine(fmt.Sprintf("line-%d", i))
	}
	lines := rb.Lines()
	want := []string{"line-3", "line-4", "line-5"}
	if len(lines) != len(want) {
		t.Fatalf("Lines() len = %d, want %d", len(lines), len(want))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("Lines()[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestSubscribeSeqDeliversSequence(t *testing.T) {
	var mu sync.Mutex
	var seqs []uint64
	var texts []string

	unsub := SubscribeSeq(func(seq uint64, line string) {
		mu.Lock()
		seqs = append(seqs, seq)
		texts = append(texts, line)
		mu.Unlock()
	})
	defer unsub()

	base := LogBuf.Seq()
	emit("seq-sub-a\n")
	emit("seq-sub-b\n")

	mu.Lock()
	defer mu.Unlock()
	if len(seqs) != 2 {
		t.Fatalf("received %d events, want 2", len(seqs))
	}
	if seqs[0] != base+1 || seqs[1] != base+2 {
		t.Fatalf("seqs = %v, want [%d %d]", seqs, base+1, base+2)
	}
	if texts[0] != "seq-sub-a\n" {
		t.Fatalf("first line = %q", texts[0])
	}
}

func TestSubscribeSeqUnsubscribeStopsDelivery(t *testing.T) {
	var mu sync.Mutex
	count := 0
	unsub := SubscribeSeq(func(uint64, string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	emit("before-unsub\n")
	unsub()
	emit("after-unsub\n")

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("callback invoked %d times, want 1 (leaked subscriber)", count)
	}
}

func TestSubscribeAndSubscribeSeqCoexist(t *testing.T) {
	// The plain Subscribe path is still used elsewhere; adding SubscribeSeq must
	// not starve it.
	var mu sync.Mutex
	plain, withSeq := 0, 0

	unsubPlain := Subscribe(func(string) {
		mu.Lock()
		plain++
		mu.Unlock()
	})
	defer unsubPlain()

	unsubSeq := SubscribeSeq(func(uint64, string) {
		mu.Lock()
		withSeq++
		mu.Unlock()
	})
	defer unsubSeq()

	emit("both-paths\n")

	mu.Lock()
	defer mu.Unlock()
	if plain != 1 || withSeq != 1 {
		t.Fatalf("plain=%d seq=%d, want 1 and 1", plain, withSeq)
	}
}

func TestAppendLineConcurrentSeqUnique(t *testing.T) {
	// AppendLine must assign each caller a distinct sequence; a Write-then-Seq()
	// implementation would hand out duplicates here.
	rb := NewRingBuffer(256)
	const n = 200

	var wg sync.WaitGroup
	got := make([]uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = rb.AppendLine("concurrent\n")
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, s := range got {
		if s == 0 {
			t.Fatal("got seq 0, sequences are 1-based")
		}
		if seen[s] {
			t.Fatalf("duplicate seq %d assigned", s)
		}
		seen[s] = true
	}
	if rb.Seq() != n {
		t.Fatalf("final Seq() = %d, want %d", rb.Seq(), n)
	}
}
