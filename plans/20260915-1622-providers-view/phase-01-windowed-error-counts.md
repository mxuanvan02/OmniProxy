# Phase 01 — Windowed error counts

**Priority:** Critical — everything else renders against this field.
**Status:** Not started
**Depends on:** nothing
**Blocks:** phase 03 (the `errors?: number` field on the wire), and therefore 04, 05, 06.

## Context links

- `plans/20260915-1622-providers-view/design.md` §4A
- `proxy/usage_tracker.go`

## Overview

`PeriodSummary` records requests, tokens and cost but nothing about failure. A
failed request still contributes whatever tokens the upstream reported before it
died, so a vendor can show a healthy-looking token count while every call it
served errored. Today the only error figure the API exposes per account is the
cumulative `errorCount` from config, which cannot be windowed: it is a lifetime
counter with no time dimension.

This phase adds the field and the two places that must accumulate it.

## Key insights

**There are two merge functions, and patching only the first is the failure mode
this phase exists to prevent.**

- `addToSummaryMap` (`proxy/usage_tracker.go:730`) writes one record into a
  per-day breakdown map. Called at `:472-478` for `ByModel`, `ByAccount`,
  `ByAPIKey`, `ByEndpoint`.
- `mergeSummaryMapInto` (`:762`) folds one day's breakdown map into a period's.
  Called at `:828-831`.

A 24h request is served from a single day bucket, so it would read errors
correctly with only the first change. A 7d/30d/60d request is served by folding
several day buckets, so without the second change `Errors` stays 0 — and "0
errors" is indistinguishable from "no errors happened", which is the most
dangerous possible wrong answer for a health page. `TestMergeSummaryMapIntoCarriesErrors`
below is the regression test for exactly that.

Because `PeriodSummary` is the element type of all four breakdown maps, error
counts arrive for free per model, per account, per API key and per endpoint. No
new route is needed: `GET /admin/api/usage/stats?period=P` starts answering the
question it was already being asked.

Accepted period values, verified against both `getPeriodCutoff` (`:928-946`) and
`dailyCutoffDate` (`:840-858`): `all`, `today`, `24h`, `7d`, `30d`, `60d`.

> **Do not use `1h`.** `getPeriodCutoff` defaults an unknown value to 24h while
> `dailyCutoffDate` defaults it to 7 days, so `1h` yields a 24h recent-request
> feed and a 7-day breakdown under one label. `UsageView.tsx:9` already offers
> `1h` and is already wrong because of this; that is a pre-existing bug outside
> this plan's scope. Phase 4's period selector must not inherit it.

## Requirements

**Functional**
- A failed request increments an error counter on its day bucket.
- Requested periods longer than one day carry the summed error count.
- The field is absent from JSON when zero, so the payload does not grow for the
  common case.

**Non-functional**
- No change to any existing counter: `Requests`, token and cost totals must be
  byte-identical for the same input.
- Three lines of production code, one new test file.

## Architecture

```
RequestRecord{Status: "error"}
        │
        ▼
updateDailyLocked ──► addToSummaryMap(day.ByAccount, id, r)   ← edit 2: s.Errors++
        │
        ▼
   dailyData["2026-09-15"] = &PeriodSummary{..., ByAccount: {id: &PeriodSummary{Errors: 1}}}
        │
        ▼
sumDailyTotalsLocked(period) ──► mergeSummaryMapInto(stats.ByAccount, day.ByAccount)  ← edit 3: d.Errors += s.Errors
        │
        ▼
GET /admin/api/usage/stats?period=7d → byAccount[id].errors
```

## Related code files

- **Modify:** `proxy/usage_tracker.go` — `PeriodSummary` struct, `addToSummaryMap`, `mergeSummaryMapInto`
- **Create:** `proxy/usage_errors_test.go`
- **Delete:** none

## Implementation steps

### Step 1: Write the failing tests

Create `proxy/usage_errors_test.go`:

```go
package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// A failed request must be counted as an error on the day bucket. The tokens it
// reported before failing are still summed, so Requests alone cannot distinguish
// a healthy account from a broken one.
func TestAddToSummaryMapCountsErrors(t *testing.T) {
	m := map[string]*PeriodSummary{}
	addToSummaryMap(m, "acct-1", RequestRecord{Status: statusError, InputTokens: 12})
	addToSummaryMap(m, "acct-1", RequestRecord{Status: statusSuccess, InputTokens: 30})

	got := m["acct-1"]
	if got == nil {
		t.Fatal("no summary written for acct-1")
	}
	if got.Requests != 2 {
		t.Fatalf("requests = %d, want 2", got.Requests)
	}
	if got.Errors != 1 {
		t.Fatalf("errors = %d, want 1", got.Errors)
	}
	if got.PromptTokens != 42 {
		t.Fatalf("promptTokens = %d, want 42 — the error path altered token accounting", got.PromptTokens)
	}
}

// The regression this field exists for. A multi-day period is served by folding
// several day buckets through mergeSummaryMapInto, so counting errors only in
// addToSummaryMap leaves 24h correct and 7d silently reporting zero.
func TestMergeSummaryMapIntoCarriesErrors(t *testing.T) {
	day1 := map[string]*PeriodSummary{}
	day2 := map[string]*PeriodSummary{}
	addToSummaryMap(day1, "acct-1", RequestRecord{Status: statusError})
	addToSummaryMap(day2, "acct-1", RequestRecord{Status: statusError})
	addToSummaryMap(day2, "acct-1", RequestRecord{Status: statusSuccess})

	period := map[string]*PeriodSummary{}
	mergeSummaryMapInto(period, day1)
	mergeSummaryMapInto(period, day2)

	got := period["acct-1"]
	if got == nil {
		t.Fatal("no summary written for acct-1")
	}
	if got.Requests != 3 {
		t.Fatalf("requests = %d, want 3", got.Requests)
	}
	if got.Errors != 2 {
		t.Fatalf("errors = %d, want 2 — mergeSummaryMapInto dropped the error count", got.Errors)
	}
}

// Errors is omitempty so the payload does not grow for the common all-success
// case. The frontend types it optional for this reason, and reads a missing key
// as zero.
func TestPeriodSummaryOmitsZeroErrors(t *testing.T) {
	data, err := json.Marshal(PeriodSummary{Requests: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "errors") {
		t.Fatalf("zero error count was serialised: %s", data)
	}

	data, err = json.Marshal(PeriodSummary{Requests: 1, Errors: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"errors":3`) {
		t.Fatalf("non-zero error count missing from %s", data)
	}
}
```

### Step 2: Run to verify they fail

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'TestAddToSummaryMapCountsErrors|TestMergeSummaryMapIntoCarriesErrors|TestPeriodSummaryOmitsZeroErrors' -v
```

Expected: compile failure — `got.Errors undefined (type *PeriodSummary has no field or method Errors)`.

### Step 3: Add the field

In `proxy/usage_tracker.go`, in the `PeriodSummary` struct, immediately after the
`EstimatedCacheCreateTokens` line (`:101`) and before the `By*` maps (`:103`):

```go
	// Errors counts failed requests in this bucket. Failure is not derivable
	// from the counters above: a request that fails still records whatever
	// tokens the upstream reported first, so a healthy-looking token total can
	// sit next to a 100% failure rate.
	Errors int `json:"errors,omitempty"`
```

### Step 4: Count it per record

In `addToSummaryMap` (`:730`), immediately after `s.Requests++` (`:739`):

```go
	s.Requests++
	if r.Status == statusError {
		s.Errors++
	}
```

### Step 5: Carry it through the period merge

In `mergeSummaryMapInto` (`:762`), immediately after `d.Requests += s.Requests` (`:772`):

```go
		d.Requests += s.Requests
		d.Errors += s.Errors
```

### Step 6: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'TestAddToSummaryMapCountsErrors|TestMergeSummaryMapIntoCarriesErrors|TestPeriodSummaryOmitsZeroErrors' -v
```

Expected: 3 PASS.

### Step 7: Verify nothing else moved

```bash
cd /Users/van/Tools/OmniProxy
GOCACHE="$TMPDIR/gocache" go test ./proxy/ -run 'Usage|Summary|Aggregation' ./proxy/ 2>&1 | tail -20
```

Expected: the pre-existing usage/aggregation tests still pass. The only failure
tolerated in this package is `TestSearchAdaptersUseNativeContracts/jina-reader`,
which needs DNS.

### Step 8: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add proxy/usage_tracker.go proxy/usage_errors_test.go
git commit -F - <<'EOF'
feat(usage): count failed requests in the period breakdown

PeriodSummary carried requests, tokens and cost but nothing about failure, so
a vendor whose every call errored still showed a healthy token count — a
failed request reports whatever tokens the upstream produced first. The only
per-account error figure on the wire was the lifetime counter from config,
which has no time dimension and cannot be windowed.

Both accumulation points need the new field. addToSummaryMap writes one record
into a day bucket; mergeSummaryMapInto folds several day buckets into the
requested period. Counting only in the first leaves 24h correct and 7d
reporting zero, which reads as "no errors" rather than "not counted".

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `proxy/usage_errors_test.go` with all three tests
- [ ] Step 2: confirm the tests fail to compile
- [ ] Step 3: add `Errors` to `PeriodSummary` with its `json:"errors,omitempty"` tag
- [ ] Step 4: increment in `addToSummaryMap`
- [ ] Step 5: sum in `mergeSummaryMapInto`
- [ ] Step 6: confirm 3 PASS
- [ ] Step 7: confirm no other usage test regressed
- [ ] Step 8: commit

## Success criteria

- `TestMergeSummaryMapIntoCarriesErrors` passes. Reverting step 5 alone must make
  it fail while `TestAddToSummaryMapCountsErrors` still passes — verify this by
  hand once, then restore.
- `GET /admin/api/usage/stats?period=7d` returns `byAccount[<id>].errors` for an
  account that failed requests within the last 7 days.
- `Requests`, token and cost values are unchanged for the same input.

## Risk assessment

| Risk | Mitigation |
|---|---|
| `mergeSummaryMapInto` missed → 7d/30d read 0 errors | `TestMergeSummaryMapIntoCarriesErrors`; the "verify by hand once" step in Success criteria |
| Client divides by zero when `errors` is present but `requests` is 0 | Impossible: an error is a request, so `Errors > 0 ⇒ Requests > 0`. Phase 3 still guards the division. |
| Daily buckets persisted before this change carry no error counts, so a 30d view under-reports | Documented in design.md §4A and surfaced as a UI note in phase 4. The response means "errors since this shipped". |

## Security considerations

None. The change adds an integer counter. No credential is read, stored or
emitted, and the field is served over the existing authenticated
`/admin/api/` route, unchanged.

## Next steps

Phase 03 mirrors `errors?: number` into `PeriodSummary` in
`web-next/src/lib/api.ts`. Phase 02 is independent and can run in parallel.
