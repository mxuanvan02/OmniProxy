# Phase 06 — Compose and live refresh

**Priority:** Critical — this is the phase that makes the page exist.
**Status:** Not started
**Depends on:** phases 03, 04, 05
**Blocks:** phase 07

## Context links

- `web-next/src/components/UsageView.tsx` — the 15-line file this rewrites; note
  line 9's `1h` option, the defect phase 01 removes
- `web-next/src/App.tsx:29` — the lazy import that fixes the module's export shape
- `web-next/src/App.tsx:52` — `usagePeriod`, the state both views share
- `web-next/src/App.tsx:80-110` — `load()`: `loadCore()` runs first on every
  path, so `status` and `accounts` are already fresh for this section
- `web-next/src/App.tsx:91-95` — the `section === 'usage'` branch that fetches
  stats and chart
- `web-next/src/App.tsx:132-138` — the poll interval and its section gate
- `web-next/src/App.tsx:149-150` — where `UsageView` is rendered
- `web-next/src/components/ProvidersView.tsx:7-9` — the `PERIODS` constant being
  moved into `lib/usage.ts`, and its explanatory comment
- `web-next/src/components/ProvidersView.test.tsx:111-117` — asserts the selector
  by button name, so it survives the constant moving
- `web-next/src/components/Shell.tsx:15` — `updatedAt`, already tracked by App

## Overview

Rewrite `UsageView.tsx` as the composition root over the five components from
phases 03–05, extend `App.tsx`'s poll gate so the page refreshes itself, and move
`ProvidersView`'s period list into `lib/usage.ts` so the one list that must match
phase 01's Go set exists once.

## Key insights

**1. `UsageView.tsx` must keep exporting a named `UsageView`.**
`App.tsx:29` lazy-loads it as
`import('./components/UsageView').then(m => ({ default: m.UsageView }))`, and the
built chunk is already committed as `webnext/dist/assets/UsageView-f3RAeHoC.js`.
Renaming the export, or making it a default export, breaks the page at runtime
with no type error — the failure appears only in the browser. Keep the named
export, keep the file name, do not add a second barrel file.

**2. `status` reaches this section for free.** `load()` awaits `loadCore()`
(`App.tsx:84`) on every path, and `loadCore` sets `accounts`, `status` and
`capabilities` — including for `section === 'usage'`. So the hero's credential and
model sub-stats need no new fetch; they need `status` passed as a prop, which
`App.tsx:150` currently omits.

**3. The five-minute window should be measured against `updatedAt`, not
`Date.now()`.** `App.tsx` already sets `updatedAt` on each successful load
(`:103`) and passes it to `Shell`. Using it means the window is relative to the
data the panel is actually showing, re-evaluates once per poll rather than on
every unrelated re-render, and is trivially testable. It is `null` before the
first load completes, so `UsageView` falls back to `Date.now()` in that case —
and the panel's disclosure covers the restart/empty case anyway.

**4. The poll gate is the existing one, widened by one section.** Today
`App.tsx:133` refreshes only for `accounts` and `overview`. Add `'usage'`. Do not
add a second interval, a `setTimeout` chain, or an SSE subscription: the
requirement is to follow the pattern the app already has, and `document.hidden`
is already respected at `:135`.

**5. The cadence is 15s, and the payload justifies not lowering it.**
`/usage/stats` is the largest polled payload in the API — the committed handler
comment at `handler.go:6422-6425` calls it "~205 KiB every 5s", which is the
legacy client's cadence. It is ETag-wrapped, so an idle period collapses to a 304
header exchange, but the first paint after a change still moves that payload. 15s
keeps the page's existing rhythm and is honest, and the hero's cadence note says
15 giây.

**6. `1h` disappears from exactly one place.** `UsageView.tsx:9`'s
`<option value="1h">1 giờ</option>`. Phase 01 removed the backend's ability to
answer it and its three-way disagreement; this phase removes the option and the
`<select>` becomes the segmented control from phase 04's family.

**7. One period list, two views.** `ProvidersView.tsx:9` holds `PERIODS` and a
comment explaining `1h`'s absence. Move the constant to `lib/usage.ts` as
`USAGE_PERIODS` and import it in both views: the list that must match the Go set
is the exact thing that drifted into the `1h` bug, and duplicating it in two
files re-creates the risk. `ProvidersView`'s suite selects by button name
(`ProvidersView.test.tsx:115`), so it stays green.

**8. Props, not context.** Six props on a page that already receives its data by
props, matching every other view (`App.tsx:143-157`). No context, no store, no
second data path.

## Requirements

**Functional**
- `UsageView` renders, in order: `PageHeader` (title "Sử dụng", the period
  segmented control in `action`, a description naming what the page shows),
  `UsageHero`, `UsageKpiRow`, `UsageChartPanel`, `UsageModelsTable`,
  `UsageProvidersPanel`, `UsageRequestsTable`.
- `UsageView` computes nothing itself: it calls phase 02's derivations and passes
  results down, or passes `usage` where a component derives one row set.
- The period control offers exactly `USAGE_PERIODS` and calls `onPeriod`.
- `App.tsx` passes `status` and `updatedAt` to `UsageView`, and the poll interval
  covers `usage`.
- `ProvidersView.tsx` imports `USAGE_PERIODS` instead of declaring `PERIODS`.

**Non-functional**
- `UsageView.tsx` under 200 lines.
- The named `UsageView` export and the file name are unchanged.
- No new state in `App.tsx` beyond what exists.
- No second polling mechanism.

## Architecture

```
App.tsx                          UsageView
  usage ─────────────────────────►  ├── UsageHero        { stats, updatedAt }
  chart ─────────────────────────►  ├── UsageKpiRow      { cards }
  status ────────────────────────►  ├── UsageChartPanel  { usage, chart, period }
  usagePeriod ◄── onPeriod ─────────┤  ├── UsageModelsTable { usage }
  updatedAt ─────────────────────►  ├── UsageProvidersPanel { usage, now }
                                    └── UsageRequestsTable  { usage }
```

## Related code files

- **Modify (rewrite):** `web-next/src/components/UsageView.tsx`
- **Create:** `web-next/src/components/UsageView.test.tsx`
- **Modify:** `web-next/src/App.tsx` — pass `status`/`updatedAt`, add `'usage'` to
  the interval gate
- **Modify:** `web-next/src/components/ProvidersView.tsx` — import the period list
- **Modify:** `web-next/src/lib/usage.ts` — add `USAGE_PERIODS` if phase 02 named
  it differently; keep one name
- **Delete:** none

## Implementation steps

### Step 1: Write the failing composition test

`UsageView.test.tsx` asserts:

```
- renders a panel for each of the seven blocks (query each data-testid)
- renders the period control with exactly three options and marks the current one
  pressed
- offers no '1h' option and no button whose label is '1 giờ' (the phase-01 defect,
  pinned in the DOM this time)
- clicking '7 ngày' calls onPeriod('7d')
- passes period through to the chart panel's props (assert via an observable
  consequence: switch the period prop and confirm the panel re-renders with the
  new value in its caption)
- renders every block for a null usage, a null status, an empty chart and a null
  updatedAt — no crash, no 'NaN', no 'undefined'
- never renders a credential-shaped value: render a usage whose recentRequests
  carry an apiKeyId-shaped identifier and assert the raw key material marker used
  in the fixture never appears in document.body.textContent (the pattern
  ProvidersView.test.tsx:149-157 established)
```

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- UsageView.test.tsx 2>&1 | tail -20
```

Expected: fails — the current 15-line `UsageView` has none of the panels and
still renders the `1h` option.

### Step 3: Rewrite `UsageView.tsx`

Order: `PageHeader` → `UsageHero` → `UsageKpiRow` → `UsageChartPanel` →
`UsageModelsTable` → `UsageProvidersPanel` → `UsageRequestsTable`. Compute
`heroStats(usage, status)`, `kpiCards(usage, status)` and
`const now = updatedAt ?? Date.now()` once, at the top.

Delete the local `Metric` and `Empty` helpers (lines 14–15) only once nothing
else in the file uses them; each new component owns its own empty state. Keep the
"Request details … trong UI cũ" link line only if the target still exists — it
points at `/admin/#usage`, which the legacy client still serves.

The period control: a button per `USAGE_PERIODS` entry with `aria-pressed`, in
`PageHeader`'s `action` slot, styled like `ProvidersView.tsx:59` so the page's two
period selectors finally look the same.

### Step 4: Wire `App.tsx`

- Line 150: pass `status={status}` and `updatedAt={updatedAt}` to `UsageView`.
- Line 133: `if (!authed || !['accounts', 'overview', 'usage'].includes(section)) return`
  with a comment naming why 15s is kept (`/usage/stats` is the largest polled
  payload).
- Leave the `usage` branch at lines 91–95 alone — it already fetches stats and
  chart for the shared period.
- Do not touch the lazy import at line 29.

### Step 5: Move the period list into `lib/usage.ts`

Delete `PERIODS` from `ProvidersView.tsx:7-9`, import `USAGE_PERIODS` from
`../lib/usage`, and replace the local comment with a one-liner at the constant's
new home that names `usagePeriods` in `proxy/usage_tracker.go` and why `1h` is
absent. Run `ProvidersView.test.tsx` immediately after this edit and before
anything else, so a broken selector is attributed to the move rather than to
phase 03–05.

### Step 6: Run everything

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -15
npx tsc -b
npm run lint 2>&1 | tail -10
wc -l src/components/UsageView.tsx
```

Expected: all suites green including `ProvidersView`'s 11 tests; `tsc -b` silent;
oxlint silent; `UsageView.tsx` under 200 lines.

If `tsc -b` complains that a lazy-loaded module's export is missing, the named
export was lost in step 3 — fix the export, do not change `App.tsx:29`.

### Step 7: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/components/UsageView.tsx web-next/src/components/UsageView.test.tsx web-next/src/App.tsx web-next/src/components/ProvidersView.tsx web-next/src/lib/usage.ts
git commit -F - <<'EOF'
feat(web-next): compose the usage dashboard and refresh it on the app's existing poll

The page was a period select, an area chart and one table. It is now the hero
figure with its sub-stats, the cost panel with a live indicator, four KPI cards, a
chart panel with a share donut, and three tables, each a component that owns its
own empty state.

Two things changed outside the view. App now passes the status and updatedAt it
was already tracking, so the hero's credential and model counts and the
five-minute window need no new fetch, and the section joined the existing poll
gate: /usage/stats is the largest polled payload in the API, which is why the
cadence stays at 15 seconds rather than matching the legacy client's five.

The period list moved into lib/usage.ts and both views import it. That list is
what drifted into the 1h defect: the UI offered a period the backend resolved to
three different windows, and it existed in two files that could disagree again.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `UsageView.test.tsx`
- [ ] Step 2: confirm the red
- [ ] Step 3: rewrite `UsageView.tsx` (keep the named export)
- [ ] Step 4: pass `status`/`updatedAt`; widen the poll gate
- [ ] Step 5: move the period list; re-run `ProvidersView.test.tsx` alone
- [ ] Step 6: full suite, `tsc -b`, oxlint, line budget
- [ ] Step 7: commit

## Success criteria

- Seven panels render, and each renders for a null usage / null status / empty
  chart without `NaN` or a crash.
- The page offers no `1h` option, in the DOM and not only in the backend.
- Selecting a period calls `onPeriod` with the shared value and both views accept
  it (`24h | 7d | 30d` is the intersection phase 01 left).
- The section refreshes on the existing 15s interval with the `document.hidden`
  guard intact.
- The named `UsageView` export survives, and `App.tsx:29`'s lazy import is
  unchanged.
- `ProvidersView`'s 11 tests still pass.
- `UsageView.tsx` under 200 lines.

## Risk assessment

| Risk | Mitigation |
|---|---|
| The lazy import breaks because the export shape changed | Key insight 1; step 6 says to fix the export rather than `App.tsx`; `tsc -b` plus the committed `UsageView-*.js` chunk name make it visible |
| `updatedAt` is `null` on first paint, so the five-minute window is `NaN` | `updatedAt ?? Date.now()`; the null-everything test covers it |
| A second polling mechanism is added "for the chart" | Step 4 widens the existing gate only; the requirement is stated in the phase brief |
| Moving `PERIODS` breaks the providers selector | Step 5 runs that suite alone, before anything else |
| `1h` reappears in a label while the option is removed | The test asserts both the option list and the absence of the "1 giờ" label |
| The page's width overflows when tables sit beside the chart | Every block stacks below `xl` and lives inside `Shell`'s `overflow-auto` main (`Shell.tsx:27`) |
| `UsageView.tsx` over 200 lines from inline layout | Components already own their internals; if it still overruns, extract the `PageHeader` + description into `UsagePageHeader.tsx` rather than trimming panels |
| A 205 KiB payload every 15s on a busy install | Already the app's cadence for other sections and ETag-wrapped; noted rather than changed, and lowering it would be a separate decision |

## Security considerations

- **No new fetch path, no new token use.** `UsageView` receives everything by
  prop; only `App.tsx`'s existing `load()` touches `api`.
- **No credential-shaped value reaches the DOM.** The composition test repeats
  `ProvidersView`'s marker assertion at page level, so a future panel cannot add
  one unnoticed.
- **`apiKeyId` is an identifier, not key material** (`proxy/auth.go:110-118`), and
  it is the only key-shaped field any panel renders.
- **The poll cannot amplify load indefinitely.** Adding one section to an interval
  guarded by `document.hidden` adds no request while the tab is backgrounded.
- **No new route is exposed**, so the admin surface's authentication boundary is
  unchanged.
- **No `dangerouslySetInnerHTML`** anywhere in the composed tree; the legacy-client
  link at the page foot is an `<a href>` to a same-origin page.

## Next steps

Phase 07 builds the bundle, embeds it in the binary and verifies the running
process is serving it. Nothing in this phase touches `webnext/dist/`.
