# Phase 02 — Usage derivations

**Priority:** Critical — every number the page shows is computed here.
**Status:** Not started
**Depends on:** phase 01 (it fixes what a period means; this module consumes a
period's worth of `UsageStats` and does not care which one)
**Blocks:** phases 03, 04, 05

## Context links

- `web-next/src/lib/api.ts:56` — `PeriodSummary`
- `web-next/src/lib/api.ts:57-60` — `RecentRequest` and its comment about the
  coarse `provider` label
- `web-next/src/lib/api.ts:61` — `UsageStats`
- `web-next/src/lib/api.ts:62` — `ChartPoint`
- `web-next/src/lib/api.ts:55` — `Status`
- `web-next/src/lib/providers.ts` — the precedent this module follows: pure
  functions, no React, own test file
- `web-next/src/lib/providers.test.ts` — the test idiom
- `web-next/src/lib/format.ts` — `compactNumber`, `exactNumber`, `relativeTime`
- `proxy/usage_tracker.go:150-160` — `UsageStats`: it does **not** embed
  `PeriodSummary`, so there is no root `errors`
- `proxy/usage_tracker.go:810-846` — `sumDailyTotalsLocked` never sums `day.Errors`
- `proxy/usage_tracker.go:431-470` — `addToSummaryMap` increments
  `PeriodSummary.Errors` when `Status == "error"`, so `byModel` is where the
  period's failures survive
- `proxy/usage_tracker.go:709-742` — `getRecentRequestsLocked` reverses the ring,
  so `recentRequests[0]` is the newest
- `proxy/usage_tracker.go:19-27` — `RequestRecord`, incl. `apiKeyId,omitempty`
- Committed `proxy/handler.go:12950-12980` — `/usage/providers` returns
  `{providers:[{id,name}]}` and nothing else

## Overview

One new module of pure functions, `web-next/src/lib/usage.ts`, plus its test.
Nothing renders here, nothing fetches: the components in phases 03–05 receive the
output. Placing the arithmetic here is what makes the wrong-number bugs testable
without a DOM — and those bugs are the ones a dashboard actually ships.

## Element-by-element source table

**Legend:** *sourced* = read straight off a field; *derived* = a documented
client-side aggregate of fields, with its limits; *dropped* = cannot be sourced.

| Reference element | Source | Kind |
|---|---|---|
| Hero: big success-token number | `UsageStats.totalCompletionTokens` | sourced |
| Hero sub: total requests | `UsageStats.totalRequests` | sourced |
| Hero sub: failed requests | `Σ usage.byModel[m].errors` | derived |
| Hero sub: time of last call | `usage.recentRequests[0].timestamp` | derived (ring) |
| Hero sub: active/total credentials | `Status.available` / `Status.totalAccounts` | sourced |
| Hero sub: model count | `Status.availableModels` | sourced |
| Cost panel: headline cost | `UsageStats.totalRealCost` (USD, computed from the pricing table) | sourced |
| Cost panel: secondary | `UsageStats.totalCost` (legacy upstream-reported credits) | sourced |
| Cost panel: live indicator | `usage.activeRequests.length > 0` | derived |
| KPI 1: credentials ready | `Status.available` of `Status.totalAccounts` | sourced |
| KPI 2: cached tokens | `UsageStats.totalCacheReadTokens` | sourced |
| KPI 3: failed requests | `Σ usage.byModel[m].errors` | derived |
| KPI 4: "unbilled amount" | — | **dropped** |
| KPI 4 replacement: effective tokens | `UsageStats.totalEffectiveTokens` | sourced |
| Chart: bar series | `ChartPoint.tokens` \| `ChartPoint.cost` for each `label` | sourced |
| Chart: stacked success/error bars | — | **dropped** (no per-bucket field) |
| Chart: period error annotation | `Σ usage.byModel[m].errors` | derived |
| Donut: slices | `byModel` \| `byAccount` \| `byApiKey` \| `byEndpoint` → `promptTokens+completionTokens` \| `realCost ?? cost` | sourced |
| Donut: centre total | `totalPromptTokens+totalCompletionTokens` \| `totalRealCost` | sourced |
| Models table: rank | index of the sorted list | derived |
| Models table: model | key of `byModel` | sourced |
| Models table: tokens | `promptTokens + completionTokens` | sourced |
| Models table: share bar | `requests / usage.totalRequests` | derived |
| Models table: error count | `byModel[m].errors ?? 0` | sourced |
| Models table: last call | newest `recentRequests` timestamp matching `model` | derived (ring) |
| Providers panel: rows | `usage.recentRequests` filtered to the last 5 min, grouped by `accountId` | derived (ring) |
| Providers panel: `/usage/providers` | — | **dropped**, see below |
| Requests table: time | `recentRequests[i].timestamp` | sourced |
| Requests table: key | `recentRequests[i].apiKeyId` | sourced, needs a type addition |
| Requests table: model | `recentRequests[i].model` | sourced |
| Requests table: credential | `recentRequests[i].accountName ?? accountId.slice(0,8)` | sourced |
| Requests table: result | `recentRequests[i].status` + `.error` | sourced |
| Requests table: call count | — | **dropped** (one record per call ⇒ always 1) |
| Requests table: total tokens | `inputTokens + outputTokens` | sourced |
| Requests table: cost | `recentRequests[i].realCost` | sourced |

### The four that cannot be sourced

1. **"Unbilled amount" (KPI 4).** Nothing in the API separates billed from
   unbilled work. `totalCost` is what the upstream reported as credits;
   `totalRealCost` is computed locally from the pricing table. Neither is a
   billing state, so the card is dropped rather than mislabelled. Replaced by
   `totalEffectiveTokens` — "Token hiệu dụng (đã trừ cache)", the
   `(input − cached) + output` figure `RequestRecord.EffectiveTokens` defines.
2. **Stacked success/error bars.** `ChartDataPoint` carries no per-bucket count.
   The period-level failure total is annotated in the panel header instead; the
   follow-up cost is recorded in `plan.md`'s known gap.
3. **Per-row call count.** The ring holds one record per request, so the column
   would be a constant `1`. Dropped — a dashboard column that can only ever say
   1 is noise.
4. **`/usage/providers` as the providers table's source.** The route returns
   `{providers:[{id,name}]}` only (`handler.go:12950-12980`): no counts, no
   timestamps, and its `name` is the coarse routing family, so every external
   vendor collapses into one row. A "last 5 minutes" table cannot be built from
   it, and calling it would produce a table that is wrong in two ways at once.
   The panel is built from `usage.recentRequests` instead.

### Per-row `key` needs a one-line type addition

`RequestRecord` sets `APIKeyID` on both the success and the error append path
(committed `handler.go:4759` and `:4828`), and the value is `config.ApiKeyEntry.ID`
— an identifier, not the secret (`proxy/auth.go:110-118`). The TypeScript
`RecentRequest` interface simply never declared it. Phase 02 adds
`apiKeyId?: string` and `RecentRequest`'s documented field list stays accurate.
The legacy client already renders `byApiKey` keys in a column labelled "API Key"
(`web/usage.js:1017-1022`), so this exposes nothing new. `omitempty` means the
field is absent whenever no admin key was configured: render `'—'`, never
`undefined`.

## Key insights

**1. Sum `byModel` for the period's failures, and never read `usage.errors`.**
`UsageStats` does not embed `PeriodSummary` (`usage_tracker.go:150-160`), so
`errors` is `undefined` at the root whatever the browser's type says — the
TypeScript `interface UsageStats extends PeriodSummary` at `api.ts:61` is more
generous than the wire. `addToSummaryMap` is the only place a failure is counted,
and its per-day maps are merged into `byModel` by `mergeSummaryMapInto`
(`sumDailyTotalsLocked:838-841`). The caveat to surface in the panel caption:
days persisted before `Errors` existed contribute nothing, the same caveat
`ProvidersView` already discloses.

**2. The ring is the only time series the client has, so say so.** Every element
marked *derived (ring)* comes from a 500-record buffer that is empty after a
restart and is not a window (see `ProvidersView`'s "Lỗi gần đây" caption). The 5
minute filter is honest only while fewer than 500 requests landed in those five
minutes; past that the window silently truncates to the newest 500. The panel
says this, and the derivation returns the count it actually used so the test can
pin it.

**3. Attribute by `accountId`, never by `provider`.** `api.ts:57-59` warns that
`provider` on a record is the coarse routing family ("External OpenAI"), shared
by every external vendor. Grouping the providers panel by it would produce one
row labelled with a family rather than a credential. `ProvidersView`'s test suite
already pins this rule for its own panel; phase 05 pins it again.

**4. `Date.parse('')` is `NaN`, so a timestamp helper must return `null`.** A
record with no timestamp would otherwise sort to the top of every "newest"
comparison. `relativeTime` (`format.ts:26-27`) already renders `'—'` for a falsy
input, so the helper's contract is `number | null` and `null` is what reaches it.

**5. A zero total must not divide.** `share` and the bar width are
`total > 0 ? value / total : 0`, not `value / total` — a fresh deployment has
`totalRequests === 0` and `NaN` in a `style.width` produces an invalid DOM value.
This is the single most likely rendering bug in the phase, so it gets its own
test.

**6. `1h` is gone, but the module still takes `period: string`.** The period is
App state that the URL does not carry; the module does not validate it. It only
exports the list the selectors must render (Key insight 7).

**7. One period list, imported by both views.** `ProvidersView.tsx:9` holds
`PERIODS = [['24h','24 giờ'],['7d','7 ngày'],['30d','30 ngày']]` with a comment
explaining `1h`'s absence. Phase 06 moves that constant into this module and has
both views import it, so the list that must match the Go set exists once. The
comment moves with it, updated to name phase 01's variable.

## Requirements

**Functional**
- `USAGE_PERIODS: ReadonlyArray<readonly [string, string]>` — the periods the
  backend serves, with Vietnamese labels. `{'24h','7d','30d'}`: the intersection
  of the three surfaces' supported set that both admin views honour.
- `USAGE_DIMENSIONS` — `model | account | apiKey | endpoint` with labels.
- `UsageMetric = 'tokens' | 'cost'`.
- `heroStats(usage, status) → HeroStats` with
  `{ successTokens, totalRequests, failedRequests, lastCallAt, activeRequests, activeCredentials, totalCredentials, modelCount }`.
- `periodErrors(usage) → number` — the `byModel` sum, `0` for a null usage.
- `kpiCards(usage, status) → KpiCard[]` — exactly four, in reference order.
- `modelRows(usage, limit) → ModelRow[]`, sorted by requests descending, with
  `share` a 0..1 fraction.
- `recentWindow(usage, minutes, now) → { rows: WindowRow[]; truncated: boolean }`,
  grouped by `accountId`, newest first.
- `requestRows(usage, limit) → RequestRow[]`, newest first.
- `shareByDimension(usage, dimension, metric) → { slices; total }`, slices sorted
  descending, each `value > 0`.
- `parseRecordTime(ts?: string) → number | null` — unix seconds, `null` when the
  string is absent or unparseable.
- Pure: no React import, no `api` import, no `Date.now()` inside a function that
  a test needs to pin (pass `now` in).

**Non-functional**
- No new dependency.
- Under 200 lines; if it overruns, split the `by*` dimension helpers out rather
  than shortening the table map.
- Every label Vietnamese, every comment English.

## Architecture

```
App.tsx ──props──► UsageView (phase 06)
                     └─ lib/usage.ts ──► phases 03/04/05 components
                          ├── heroStats / kpiCards        (status + usage)
                          ├── modelRows / recentWindow    (usage.recentRequests)
                          ├── requestRows / periodErrors  (usage)
                          └── shareByDimension            (usage.by*)
```

## Related code files

- **Create:** `web-next/src/lib/usage.ts`
- **Create:** `web-next/src/lib/usage.test.ts`
- **Modify:** `web-next/src/lib/api.ts` — add `apiKeyId?: string` to
  `RecentRequest`, one line, with a comment naming the append paths
- **Delete:** none

## Implementation steps

### Step 1: Write the failing test

Create `web-next/src/lib/usage.test.ts`. Build the fixtures with the same
factory style as `ProvidersView.test.tsx` (`summary()`), so a test can express
"requests served, `errors` field absent" — a server older than the field omits a
zero. Fixtures must cover: a healthy model, a failed model, a model absent from
`recentRequests`, a record with no timestamp, a record with no `apiKeyId`, a
record whose `provider` is the coarse family, `totalRequests: 0`, and
`recentRequests: []`.

The assertions, one per group:

```
USAGE_PERIODS
  - never contains '1h'
  - contains exactly the values phase 01's Go set serves for this UI

periodErrors
  - sums byModel errors across models
  - returns 0 for a null usage
  - returns 0, not NaN, when no summary carries an errors key
  - does not read a root-level errors (a fixture with root errors: 99 and no
    byModel errors must still return 0 — the root field does not exist)

heroStats
  - successTokens is usage.totalCompletionTokens
  - lastCallAt is the first parseable recentRequests timestamp
  - lastCallAt is null when the newest record has no timestamp but an older one
    parses (proves it scans, not just reads index 0)
  - activeCredentials / totalCredentials / modelCount come from Status
  - every field is 0 / null / 0 for a null usage and a null status

kpiCards
  - returns exactly four cards
  - the fourth is the effective-token replacement and its note is non-empty
  - contains no card whose label mentions "unbilled"

modelRows
  - sorted by requests descending
  - share is requests / totalRequests, and 0 (not NaN) when totalRequests is 0
  - errors is 0 for a summary without the field
  - lastCallAt is the newest matching recentRequests timestamp, null when the
    ring holds no record for that model
  - respects limit

recentWindow
  - excludes a record older than the window
  - excludes a record whose timestamp is absent or unparseable
  - groups by accountId, so two accounts with the same coarse provider label
    produce two rows and no row is labelled 'External OpenAI'
  - truncated is true when the ring holds exactly limit records inside the window
  - calls counts only in-window records; errors only in-window failures

requestRows
  - newest first
  - tokenTotal is inputTokens + outputTokens
  - key is '—' when apiKeyId is absent
  - credential falls back to accountId.slice(0, 8) when accountName is absent
  - respects limit

shareByDimension
  - slices are sorted descending and every value is > 0
  - metric 'cost' reads realCost, falling back to cost
  - total is the sum of the slices, not of the whole map (a dimension dropped
    for a zero value must not sit in the denominator)
  - returns an empty slice list and total 0 for a null usage

parseRecordTime
  - returns unix seconds for an RFC3339 string
  - returns null for undefined, '', and 'not a date'
```

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- usage.test.ts 2>&1 | tail -20
```

Expected: fails to resolve `./usage`.

### Step 3: Write the module

Implement the exports listed in Requirements. Two points of care:

- `recentWindow` computes `truncated` as
  `inWindow.length >= RING_CAPACITY && usage.recentRequests.length >= RING_CAPACITY`
  with `const RING_CAPACITY = 500` exported so the panel can name the limit in
  its disclosure and the test can pin it. Read `RING_CAPACITY`'s comment against
  `usage_tracker.go:194` (`ringCap: 500`).
- `shareByDimension` filters `value > 0` **before** summing, so the centre total
  is the sum of what the donut actually draws.

Add to `web-next/src/lib/api.ts`:

```ts
/** Set by the tracker on both append paths (proxy/handler.go:4759 success,
 *  :4828 error) from config.ApiKeyEntry.ID — an identifier, never the key
 *  material. `omitempty` upstream, so a deployment with no admin keys sends
 *  nothing and the column renders '—'. */
apiKeyId?: string
```

### Step 4: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- usage.test.ts 2>&1 | tail -25
```

Expected: all pass.

### Step 5: Run the whole suite, typecheck and lint

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -12 && npx tsc -b && npm run lint 2>&1 | tail -10
```

Baseline (measured while writing this plan): 8 files, 50 tests, all passing.
Expected after this phase: 9 files, 50 + the new tests, all passing; `tsc -b`
silent; oxlint silent.

### Step 6: Confirm the line budget

```bash
cd /Users/van/Tools/OmniProxy/web-next && wc -l src/lib/usage.ts
```

Expected: under 200. If over, split the dimension helpers into
`src/lib/usage-dimensions.ts` (that is a genuinely separate concern: mapping a
dimension name to a `by*` map) and re-export — do not shorten the source table.

### Step 7: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/lib/usage.ts web-next/src/lib/usage.test.ts web-next/src/lib/api.ts
git commit -F - <<'EOF'
feat(web-next): derive every usage figure in one testable module

The dashboard needs period failures, per-model share, a five-minute window and a
share-by-dimension breakdown, and none of them is a field on the wire. Computing
them inside components would make the four bugs that matter — a zero read as
missing, a share against a zero total, an aggregate attributed by the coarse
provider label, a NaN timestamp sorting to the top — reachable only through a
DOM.

Failures come from summing byModel. UsageStats does not embed PeriodSummary, so
there is no root errors field, however the TypeScript interface reads, and
addToSummaryMap is the only place a failure is counted. The five-minute panel is
built from the recent-requests ring rather than /usage/providers, which returns
provider names and nothing else.

RecentRequest gains apiKeyId. The tracker has always set it on both append paths
and it is an ApiKeyEntry identifier, not key material; the interface simply never
declared it.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `usage.test.ts` with the assertion groups above
- [ ] Step 2: confirm it fails to resolve `./usage`
- [ ] Step 3: write `usage.ts`; add `apiKeyId` to `api.ts`
- [ ] Step 4: `usage.test.ts` green
- [ ] Step 5: full suite, `tsc -b`, oxlint clean
- [ ] Step 6: under 200 lines
- [ ] Step 7: commit

## Success criteria

- `periodErrors` returns 0 for a fixture whose only error signal is a root-level
  `errors: 99` — proving the phantom field is not read.
- `recentWindow` never emits a row labelled with a routing family, and reports
  `truncated` when the ring caps the window.
- No `share` or bar width is ever `NaN` for a zero total.
- `lastCallAt` is `null` rather than `NaN` for an unparseable timestamp.
- `USAGE_PERIODS` contains no `'1h'`.
- 50 pre-existing tests still pass; `tsc -b` and oxlint silent.

## Risk assessment

| Risk | Mitigation |
|---|---|
| Reading `usage.errors` (present in the TS type, absent on the wire) and rendering `undefined` as "NaN" | `periodErrors` test with `errors: 99` at the root and none in `byModel` must return 0 |
| Share or width `NaN` on a fresh deployment (`totalRequests === 0`) | Dedicated test; guard is `total > 0 ? v / total : 0` |
| Providers 5-minute panel grouping by the coarse `provider` label | Test asserts two accounts with the same `provider` yield two rows and no row equals `'External OpenAI'` |
| Ring truncation presented as a true 5-minute window | `truncated` is returned and pinned by a test at exactly `RING_CAPACITY` in-window records; phase 05 renders the disclosure |
| `lastCallAt` `NaN` breaking the "newest" comparison | `parseRecordTime` returns `null`; a test covers index-0-unparseable |
| A record timestamp compared as a string against a computed cutoff | The module converts to unix seconds once, via `parseRecordTime`, and compares numbers |
| Over 200 lines | Split path named in step 6 |

## Security considerations

- **No credential is read or rendered.** `apiKeyId` is `ApiKeyEntry.ID`, an
  identifier; the module never touches key material, and `ProvidersView`'s
  existing suite already pins that the raw fields never reach the DOM.
- **No `dangerouslySetInnerHTML`, no URL building.** Upstream error strings and
  model names pass through as data; React escapes them at render.
- **No fetch, no token access.** The module imports types only, so it cannot
  reach `sessionStorage` or attach the admin header.
- **`truncated` is surfaced, not hidden.** A silent truncation would let an
  operator read an empty 5-minute panel as "no traffic", which is a wrong
  operational conclusion — the honesty here is a correctness requirement, not
  politeness.

## Next steps

Phases 03, 04 and 05 each import from here and render. Phase 06 moves
`ProvidersView`'s `PERIODS` constant into this module (Key insight 7) and edits
`usage.test.ts` if the export name changes.
