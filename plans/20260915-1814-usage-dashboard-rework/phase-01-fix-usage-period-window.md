# Phase 01 — Fix the usage period window

**Priority:** Critical — it is the only backend change, and it decides what the
rest of the plan is allowed to offer in the period selector.
**Status:** Not started
**Depends on:** nothing
**Blocks:** phases 06 and 07 (the UI's period list must match this set)

## Context links

- `proxy/usage_tracker.go:938-957` — `getPeriodCutoff` (the switch the brief
  called "`GetStats` ~line 940"; `GetStats` itself has no switch, it delegates to
  this and to `dailyCutoffDate`)
- `proxy/usage_tracker.go:850-869` — `dailyCutoffDate`, the second switch
- `proxy/usage_tracker.go:634-648` — `GetChartData`, the third switch
- `proxy/usage_tracker.go:649-683` / `684-702` — `bucketByHour` / `bucketByDay`
- `proxy/usage_tracker.go:542-546` — `GetStats` calling both aggregators
- `proxy/usage_tracker.go:810-846` — `sumDailyTotalsLocked`, the consumer of
  `dailyCutoffDate`; note it never sums `day.Errors`, so `UsageStats` has no
  root-level error total (phase 02 depends on that fact)
- `proxy/usage_timezone_test.go` — `newTestTracker()`, `pinTZ()`, `addRecordAt()`
- Committed `proxy/handler.go:12687-12703` — reads `?period=` raw and defaults
  empty to `24h` for stats but `7d` for the chart
- Committed `proxy/handler.go:12859` and `:12952` — `GetStats("all")`, called by
  `/usage/request-details` and `/usage/providers`
- `web/usage.js:1512` — the legacy client's list, `['24h','7d','30d','all']`:
  it never offered `1h`, so the new client's `1h` option is a regression

## Overview

The new client offers a "1 giờ" (`1h`) option. No backend surface implements it.
Worse, the three switches that resolve a period each carry their **own** default,
so one selection produces three windows at once:

| Surface | Switch | `1h` today |
|---|---|---|
| `recentRequests` feed | `getPeriodCutoff` | default → `now-24h` |
| period totals + `byModel`/`byAccount`/`byApiKey`/`byEndpoint` | `dailyCutoffDate` | default → `now-6d` |
| chart buckets | `GetChartData` | default → `bucketByDay(now, 7)` |

So the tables show 7 days, the recent-requests feed 24 hours, and the chart 7
days, under one label. The fix: one supported set, one resolver, every switch
going through it.

## Key insights

**1. The fix cannot simply delete `all` or `60d`.** `sumDailyTotalsLocked` feeds
from `dailyCutoffDate`, and two committed endpoints call `GetStats("all")`
outside any UI (`/usage/request-details`, `/usage/providers` — the ring is
"all" by definition there). Narrowing `getPeriodCutoff`/`dailyCutoffDate` to the
three periods the UI shows would silently change both routes' meaning. The
supported set is therefore the **union** of what the aggregators already
implement; nothing that worked stops working.

**2. `1h` cannot be implemented, which is why it must be removed.** `dailyData`
is keyed by the UTC *date* string (`usage_tracker.go:434-437`), and
`sumDailyTotalsLocked` sums whole days, so no sub-day total can be produced — a
`1h` window would have to report up to 24 hours of the current day. The chart has
no bucket shape finer than `bucketByHour`'s fixed 24. Implementing `1h` means a
new sub-day aggregation path and a new bucket function: well beyond "one bug
fix". Removing the option is the honest change, and it restores what the legacy
client already did.

**3. The resolver's fallback must be one value.** Today `getPeriodCutoff` falls
back to 24h and `dailyCutoffDate` to 7 days. After the fix an unsupported value
resolves to `24h` on every surface, which is what makes the surfaces agree for
any input, not just for the values the UI sends.

**4. A residual mismatch stays, and it is not this bug.** `dailyCutoffDate("24h")`
returns *yesterday's date*, so the totals include part of yesterday (up to 48h of
days) while `getPeriodCutoff("24h")` is an exact rolling 24h. That asymmetry is
documented at `usage_tracker.go:845-849` as deliberate day-granularity behaviour.
The requirement is that the surfaces agree on the supported **set**; making the
24h semantics identical would change period totals for every existing reader, so
it stays. Record it, do not fix it.

**5. `all` and `60d` keep their current chart shape, deliberately.** Both fall
through to `bucketByDay(now, 7)` today. The plan makes that explicit rather than
accidental but does **not** widen it: widening is a behaviour change beyond the
one bug fix, and this UI offers neither period. See *Unresolved questions*.

**6. Two more default sites exist outside the switches.**
`apiGetUsageStats` defaults an empty `period` to `24h` and `apiGetUsageChart`
defaults it to `7d` (`handler.go:12687-12703`). Unreachable from this client
(`api.ts:97-98` always sends a value) and out of scope — `handler.go` is off
limits for this task. Note it so the next reader is not surprised.

## Requirements

**Functional**
- One exported set of supported usage periods in `proxy/usage_tracker.go`.
- One resolver mapping any input, including an unknown one, to a member of that
  set; the same answer on every surface.
- `getPeriodCutoff`, `dailyCutoffDate` and `GetChartData` each begin with that
  resolver, and each keeps an explicit arm for every member of the set.
- No member that works today stops working, and no caller's window changes —
  except that all three surfaces now answer identically for unsupported input.

**Non-functional**
- No new endpoint, no new response field, no new JSON tag.
- `ChartDataPoint` keeps exactly `label`, `tokens`, `cost`.
- Package `proxy` builds and its tests keep their current pass set.

## Architecture

```
                    ?period=<any string>
                            │
                    resolveUsagePeriod      ← the one supported set (usagePeriods)
                            │
        ┌───────────────────┼───────────────────┐
        ▼                   ▼                   ▼
  getPeriodCutoff    dailyCutoffDate      GetChartData
  (ring feed)        (dailyData totals)   (bucket shape)
```

Before, three independent `default:` arms; after, one resolver and three
exhaustive switches.

## Related code files

- **Modify:** `proxy/usage_tracker.go`
- **Create:** `proxy/usage_period_test.go`
- **Delete:** none

## Implementation steps

### Step 1: Write the failing test

Create `proxy/usage_period_test.go` (same package, so `newTestTracker()` from
`usage_timezone_test.go` is in scope):

```go
package proxy

import "testing"

// The bug: the UI offers "1 giờ", no backend surface implements it, and the
// three switches that resolve a period each carried their own default — so one
// selection produced three windows at once (recent requests 24h, the By* tables
// 7 days, the chart 7 days). These tests fail against that code.
func TestUnsupportedPeriodResolvesToTheSameWindowEverywhere(t *testing.T) {
	const bogus = "1h"

	if got, want := getPeriodCutoff(bogus), getPeriodCutoff("24h"); !got.Equal(want) {
		t.Fatalf("getPeriodCutoff(%q) = %s, want the 24h window %s", bogus, got, want)
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
// count each one produces: 24 hourly, 7 daily.
func TestHourlyAndDailyShapesArePinned(t *testing.T) {
	tr := newTestTracker()
	if got := len(tr.GetChartData("24h")); got != 24 {
		t.Fatalf(`GetChartData("24h") = %d buckets, want 24`, got)
	}
	if got := len(tr.GetChartData("7d")); got != 7 {
		t.Fatalf(`GetChartData("7d") = %d buckets, want 7`, got)
	}
}
```

`time` must be imported for the second test: `import ("testing"; "time")`.

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'TestUnsupportedPeriodResolves|TestEverySupportedPeriod|TestHourlyAndDailyShapes' -count=1 2>&1 | tail -20
```

Expected: `TestUnsupportedPeriodResolvesToTheSameWindowEverywhere` fails at the
`dailyCutoffDate` assertion (`"1h"` → `now-6d`, `"24h"` → `now-1d`) and
`TestEverySupportedPeriodIsExplicitOnEverySurface` fails to compile until
`resolveUsagePeriod`/`usagePeriods` exist. That compile error is the first red.

### Step 3: Add the set and the resolver

In `proxy/usage_tracker.go`, next to `getPeriodCutoff`:

```go
// usagePeriods is the one supported set of usage periods. Every surface that
// resolves a period — getPeriodCutoff and dailyCutoffDate for the totals and
// the recent-requests feed, GetChartData for the chart — goes through
// resolveUsagePeriod, so two surfaces can no longer silently land on different
// windows. The set is the union of what those aggregators already implement:
// nothing that worked before stops working, and nothing new is invented.
//
// "1h" is deliberately absent, and that absence is what the UI must mirror.
// dailyData is keyed by the UTC day, so no sub-day total can be aggregated from
// it, and the chart has no bucket shape finer than one hour. The UI offered it
// anyway; getPeriodCutoff then fell back to 24h while dailyCutoffDate fell back
// to 7 days, so one label showed three windows at once.
var usagePeriods = []string{"today", "24h", "7d", "30d", "60d", "all"}

// resolveUsagePeriod maps any period, including an unsupported one, to a member
// of usagePeriods. An unknown value resolves to 24h on every surface — the same
// window everywhere is the whole point.
func resolveUsagePeriod(period string) string {
	for _, supported := range usagePeriods {
		if supported == period {
			return period
		}
	}
	return "24h"
}
```

### Step 4: Route the three switches through it

Each switch gains `resolveUsagePeriod(period)` as its subject and keeps an
explicit arm per member. Concretely:

- `getPeriodCutoff` (`:938`): `switch resolveUsagePeriod(period)`; keep the six
  existing arms in their current order; replace the body of `default:` with
  `return now.Add(-24 * time.Hour)` plus a comment that it is unreachable
  (the resolver only ever returns a member).
- `dailyCutoffDate` (`:850`): `switch resolveUsagePeriod(period)`; keep the six
  arms; make `default:` return `now.AddDate(0, 0, -1).Format("2006-01-02")` —
  the **24h** cutoff, not the 7-day one. This line is the bug.
- `GetChartData` (`:634`): `switch resolveUsagePeriod(period)`; keep `today`,
  `24h`, `7d`, `30d` as they are; add
  `case "60d", "all": return t.bucketByDay(now, 60)` with a comment naming the
  choice, and make `default:` the `24h` shape. The `all`/`60d` widening is the
  one place this phase touches a shape rather than a default — see
  *Unresolved questions* if you would rather keep both at 7 buckets.

Do not reorder or reword the existing arms: several carry comments tying them to
the UTC-keyed daily buckets, and those comments are the reason the arms are
correct.

### Step 5: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'TestUnsupportedPeriodResolves|TestEverySupportedPeriod|TestHourlyAndDailyShapes|TestChartTodayMatches|TestBucketByHourTodayCovers|TestGetPeriodCutoffToday|TestBucketByDayUsesUTCDateKeys|TestSumDailyTotalsNotCappedByRing' -count=1 -v 2>&1 | tail -30
```

Expected: all pass. The four pre-existing period tests are included on purpose —
this phase re-words their subject expression, and they are the guard against a
reworded arm.

### Step 6: Run the whole package and compare against the baseline

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go build ./... && GOCACHE="$TMPDIR/gocache" go test ./proxy/ -count=1 2>&1 | grep -E "FAIL|ok " | head
```

Baseline (measured while writing this plan): `go test ./proxy/` fails **only**
`TestSearchAdaptersUseNativeContracts/jina-reader` with `url host could not be
resolved` — the sandbox has no DNS. Anything else red is this phase's regression.

### Step 7: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add proxy/usage_tracker.go proxy/usage_period_test.go
git commit -F - <<'EOF'
fix(proxy): resolve usage periods through one set so the surfaces cannot disagree

The admin client offers a "1 giờ" period that no surface implements. Because
getPeriodCutoff, dailyCutoffDate and GetChartData each carried their own default
arm, one selection produced three windows at once: the recent-requests feed over
24h, the period totals and every By* breakdown over 7 days, and the chart over 7
days.

The supported periods now live in one variable and resolve through one function,
so an unsupported value answers the same way everywhere. "1h" is not added: the
daily aggregation is keyed by UTC day and the chart has no bucket finer than an
hour, so a 1h window could only report up to 24 hours of the current day. The set
is the union of what the aggregators already served, so no existing caller's
window moves.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `proxy/usage_period_test.go` with all three tests
- [ ] Step 2: run and confirm the red (compile error, then the `dailyCutoffDate` assertion)
- [ ] Step 3: add `usagePeriods` + `resolveUsagePeriod`
- [ ] Step 4: route `getPeriodCutoff`, `dailyCutoffDate`, `GetChartData` through it
- [ ] Step 5: targeted tests pass, including the four pre-existing period tests
- [ ] Step 6: `go build ./...` clean; package failures unchanged from baseline
- [ ] Step 7: commit

## Success criteria

- `getPeriodCutoff("1h")`, `dailyCutoffDate("1h")` and `GetChartData("1h")`
  each equal their `"24h"` counterpart. One value, one window.
- Every member of `usagePeriods` is an explicit arm on all three surfaces;
  no member reaches a `default:` arm.
- `GetChartData("24h")` returns 24 buckets and `GetChartData("7d")` returns 7.
- `GetStats("all")` still returns the whole ring — checked by
  `/usage/request-details` and `/usage/providers` behaviour, which this phase
  does not touch.
- `ChartDataPoint` still declares exactly `label`, `tokens`, `cost`.
- `go build ./...` clean; the package's only failure is the pre-existing DNS one.

## Risk assessment

| Risk | Mitigation |
|---|---|
| Narrowing the set breaks `/usage/request-details` and `/usage/providers`, which call `GetStats("all")` outside any UI | The set is the union of the existing arms, so `all` stays meaningful; `TestEverySupportedPeriodIsExplicitOnEverySurface` pins it |
| `dailyCutoffDate`'s changed `default` moves a real window | Its six explicit arms are untouched, and only a value the resolver rejects ever reaches `default` |
| `UsageStats` has no root `errors`, so a "failed requests" total looks like a missing field and tempts a backend addition | Phase 02 sums `byModel[*].errors` instead; `sumDailyTotalsLocked:810-846` proves the field is absent, and `UsageStats` (`:150-160`) has no such field |
| A reworded arm silently changes the 60d/all window | Arm bodies and their comments are untouched; only the switch subject changes |
| The residual 24h day-granularity asymmetry gets "fixed" by mistake | Recorded in Key insight 4 as deliberate (`usage_tracker.go:845-849`) |

## Security considerations

- **No new input surface.** `?period=` was already raw user input reaching these
  functions; the resolver makes an unsupported value *less* reachable than the
  three unbounded fallbacks did, and returns a constant, so no value is
  concatenated into a path, query, or log line.
- **No new data exposure.** No struct gains a JSON tag and no field is added to
  any response, so the phase cannot widen what an authenticated admin sees.
- **No allocation change.** `usagePeriods` is a package-level slice of string
  literals and is never written, so it is safe under the tracker's `RWMutex`
  without taking the lock.
- The routes remain behind the admin token as before; this phase does not touch
  auth, and needs no credential to test.

## Next steps

Phase 02 imports the supported pair from `lib/usage.ts`, and phase 06 replaces
`UsageView.tsx`'s `<option value="1h">1 giờ</option>` with that list. The
residual items — whether to widen the `all`/`60d` chart window, and whether to
expose `today` — are recorded below and need the user's answer before anyone
acts on them.

## Unresolved questions

1. `all` and `60d` are now charted as 60 day-buckets instead of the 7 they fell
   back to. That is a visible change to the legacy client's chart if anyone
   selects "all" there. Keep it, or pin both to 7 explicitly? Phase 06's UI
   offers neither, so the choice is invisible to this page.
2. `today` is supported on all three surfaces (midnight-UTC start on
   `getPeriodCutoff`, today's date on `dailyCutoffDate`, hourly-from-midnight on
   the chart) but is not offered, because `usagePeriod` is shared App state and
   `ProvidersView`'s selector does not carry it (`App.tsx:148`) — selecting it on
   the usage page would leave the providers page showing a period it cannot
   display a pressed tab for.
