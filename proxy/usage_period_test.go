package proxy

import (
	"testing"
	"time"
)

// The bug: the admin client offers a "1 giờ" period that no surface
// implements. Because getPeriodCutoff, dailyCutoffDate and GetChartData each
// carried their own default arm, one selection produced three windows at once —
// the recent-requests feed over 24h, the period totals and every By* breakdown
// over 7 days, and the chart over 7 days.
//
// The two date-granular surfaces are asserted exactly; getPeriodCutoff is
// asserted with a tolerance because it reads its own time.Now() per call, so
// two calls can never be Equal however the period resolves.
func TestUnsupportedPeriodResolvesToTheSameWindowEverywhere(t *testing.T) {
	const bogus = "1h"

	if got, want := resolveUsagePeriod(bogus), resolveUsagePeriod("24h"); got != want {
		t.Fatalf("resolveUsagePeriod(%q) = %q, want the 24h answer %q", bogus, got, want)
	}

	gotCutoff, wantCutoff := getPeriodCutoff(bogus), getPeriodCutoff("24h")
	if delta := gotCutoff.Sub(wantCutoff); delta < -2*time.Second || delta > 2*time.Second {
		t.Fatalf("getPeriodCutoff(%q) = %s, want the 24h window near %s", bogus, gotCutoff, wantCutoff)
	}

	if got, want := dailyCutoffDate(bogus), dailyCutoffDate("24h"); got != want {
		t.Fatalf("dailyCutoffDate(%q) = %q, want the 24h cutoff %q", bogus, got, want)
	}

	// bucketByDay returns one point per day and bucketByHour always 24, so the
	// bucket count is what proves the chart followed the 24h shape rather than
	// landing on the 7-day default.
	tr := newTestTracker()
	if got, want := len(tr.GetChartData(bogus)), len(tr.GetChartData("24h")); got != want {
		t.Fatalf("GetChartData(%q) returned %d buckets, want the 24h shape of %d", bogus, got, want)
	}
}

// Every member of the set must be exhaustive on every surface: a member that
// falls through to a default arm is a window that disagrees with the others.
func TestEverySupportedPeriodIsExplicitOnEverySurface(t *testing.T) {
	tr := newTestTracker()
	now := time.Now().UTC()
	tr.dailyData[now.Format("2006-01-02")] = &PeriodSummary{PromptTokens: 100, CompletionTokens: 10}

	for _, period := range usagePeriods {
		if got := resolveUsagePeriod(period); got != period {
			t.Fatalf("resolveUsagePeriod(%q) = %q, want itself", period, got)
		}
		if points := tr.GetChartData(period); len(points) == 0 {
			t.Fatalf("GetChartData(%q) returned no buckets", period)
		}
		if cutoff := getPeriodCutoff(period); period != "all" && cutoff.IsZero() {
			t.Fatalf("getPeriodCutoff(%q) returned the zero time, which only \"all\" means", period)
		}
		if date := dailyCutoffDate(period); period != "all" && date == "" {
			t.Fatalf("dailyCutoffDate(%q) returned an empty cutoff, which only \"all\" means", period)
		}
	}
}

// "24h" and "7d" are the two shapes the UI leans on hardest, so pin the bucket
// count each one produces before the switch subjects are reworded: 24 hourly,
// 7 daily.
func TestHourlyAndDailyShapesArePinned(t *testing.T) {
	tr := newTestTracker()
	if got := len(tr.GetChartData("24h")); got != 24 {
		t.Fatalf(`GetChartData("24h") = %d buckets, want 24`, got)
	}
	if got := len(tr.GetChartData("7d")); got != 7 {
		t.Fatalf(`GetChartData("7d") = %d buckets, want 7`, got)
	}
}
