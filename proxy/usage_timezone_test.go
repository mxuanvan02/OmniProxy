package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pinTZ forces time.Local to a positive-offset zone for the duration of the
// test. The UTC read paths must be unaffected by it; before the fix the chart
// followed time.Local while the period totals followed the UTC daily keys.
func pinTZ(t *testing.T) {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

func newTestTracker() *UsageTracker {
	return &UsageTracker{
		ringCap:    500,
		ring:       make([]RequestRecord, 500),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
}

// addRecordAt files a record into both the ring and the UTC-keyed daily
// aggregation, the way Append does, so the chart and the period totals read the
// same event from their respective sources.
func addRecordAt(tr *UsageTracker, ts time.Time, in, out int) {
	utc := ts.UTC()
	tr.pushToRing(RequestRecord{
		Timestamp:    utc.Format(time.RFC3339),
		Model:        "claude-opus-5",
		InputTokens:  in,
		OutputTokens: out,
	})
	key := utc.Format("2006-01-02")
	day, ok := tr.dailyData[key]
	if !ok {
		day = &PeriodSummary{}
		tr.dailyData[key] = day
	}
	day.Requests++
	day.PromptTokens += in
	day.CompletionTokens += out
}

// TestChartTodayMatchesPeriodTodayInNonUTCZone is the M1 regression: at UTC+7,
// between local midnight and UTC midnight the chart's today column read the
// previous UTC day (0 tokens) while /usage?period=today reported the UTC total.
func TestChartTodayMatchesPeriodTodayInNonUTCZone(t *testing.T) {
	pinTZ(t)
	mustInitConfig(t) // GetStats reads config accounts for the name map

	tr := newTestTracker()
	now := time.Now().UTC()
	if now.Hour() < 4 {
		t.Skip("within 4h of UTC midnight: no room to place same-UTC-day fixtures")
	}
	for _, back := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		addRecordAt(tr, now.Add(-back), 1000, 25)
	}

	chartTokens := 0
	for _, p := range tr.GetChartData("today") {
		chartTokens += p.Tokens
	}

	stats := tr.GetStats("today")
	periodTokens := stats.TotalPromptTokens + stats.TotalCompletionTokens

	if chartTokens != periodTokens {
		t.Fatalf("chart-today %d != period-today %d", chartTokens, periodTokens)
	}
	if periodTokens == 0 {
		t.Fatal("period-today is 0; the fixture did not land in the current UTC day")
	}
}

// TestBucketByHourTodayCoversCurrentUTCDay pins the window explicitly: with a
// fixed now of 20:00 UTC (03:00 next-day local at UTC+7) every record of that
// UTC day must be bucketed, including the 19:00 one the local-anchored window
// dropped.
func TestBucketByHourTodayCoversCurrentUTCDay(t *testing.T) {
	pinTZ(t)

	now := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)
	tr := newTestTracker()
	for _, h := range []int{0, 12, 19} {
		addRecordAt(tr, now.Add(-time.Duration(20-h)*time.Hour), 100, 10)
	}

	points := tr.bucketByHour(now, true)
	if points[0].Label != "00:00" {
		t.Fatalf("first bucket label = %q, want 00:00", points[0].Label)
	}
	total := 0
	for _, p := range points {
		total += p.Tokens
	}
	if total != 330 {
		t.Fatalf("bucketed tokens = %d, want 330", total)
	}
	for _, h := range []int{0, 12, 19} {
		if points[h].Tokens != 110 {
			t.Fatalf("bucket %02d:00 tokens = %d, want 110", h, points[h].Tokens)
		}
	}
}

func TestGetPeriodCutoffTodayIsUTCMidnight(t *testing.T) {
	pinTZ(t)

	got := getPeriodCutoff("today")
	now := time.Now().UTC()
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("cutoff = %s, want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("cutoff location = %s, want UTC", got.Location())
	}
}

func TestBucketByDayUsesUTCDateKeys(t *testing.T) {
	pinTZ(t)

	now := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)
	tr := newTestTracker()
	tr.dailyData["2026-01-01"] = &PeriodSummary{PromptTokens: 700, CompletionTokens: 7}

	points := tr.bucketByDay(now, 7)
	last := points[len(points)-1]
	if last.Tokens != 707 {
		t.Fatalf("last bucket tokens = %d, want 707 (label %q)", last.Tokens, last.Label)
	}
}

// TestStopFlushesAndEndsPeriodicFlush covers the shutdown contract main.go
// depends on: periodicFlush returns, the last window reaches disk, and a second
// Stop is a no-op.
func TestStopFlushesAndEndsPeriodicFlush(t *testing.T) {
	dir := t.TempDir()
	tr := newTestTracker()
	tr.historyPath = filepath.Join(dir, "usage_history.json")
	tr.dailyPath = filepath.Join(dir, "usage_daily.json")
	tr.stop = make(chan struct{})
	addRecordAt(tr, time.Now().UTC(), 42, 1)
	tr.dirty = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.periodicFlush()
	}()

	tr.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("periodicFlush did not return after Stop")
	}

	for _, p := range []string{tr.historyPath, tr.dailyPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", p)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s mode = %o, want 600", p, perm)
		}
	}

	tr.mu.RLock()
	dirty := tr.dirty
	tr.mu.RUnlock()
	if dirty {
		t.Fatal("dirty still set after a successful final flush")
	}

	tr.Stop() // idempotent: must not panic on the closed channel
}
