# Phase 04 — Chart panel

**Priority:** High — the reference's most detailed block, and the one where the
API's limits bite.
**Status:** Not started
**Depends on:** phase 02 (`shareByDimension`, `periodErrors`, dimension/metric
types)
**Blocks:** phase 06

## Context links

- `web-next/src/components/UsageView.tsx:11` — the `AreaChart` being replaced
- `web-next/src/lib/api.ts:62` — `ChartPoint`: exactly `label`, `tokens`, `cost`
- `proxy/usage_tracker.go:163-167` — `ChartDataPoint`, the same three fields
- `proxy/usage_tracker.go:649-702` — `bucketByHour` / `bucketByDay`, where a
  per-bucket count would have to be counted
- `web-next/src/components/ProvidersView.tsx:58-62` — the segmented-control idiom
  (a button per period, `aria-pressed`) this phase follows for its tabs
- `plans/20260915-1814-usage-dashboard-rework/plan.md` — the known-gap note
- recharts 3.10.1 is installed; `BarChart`, `Bar`, `PieChart`, `Pie`, `Cell`,
  `ResponsiveContainer`, `Tooltip`, `XAxis`, `YAxis`, `CartesianGrid`,
  `Legend` were all verified present in the installed copy

## Overview

One component, `UsageChartPanel.tsx`, holding the panel header's controls, a bar
chart of the period's buckets, and a donut of share by the selected dimension
with the total in its centre.

## Key insights

**1. The stacked success/error bar chart cannot be drawn, and must not be faked.**
`ChartDataPoint` carries no `requests` or `errors`, and `bucketByHour` /
`bucketByDay` (`usage_tracker.go:649-702`) never count them. The reference's
signature chart layer is therefore unavailable for **every** range, not just long
ones. The panel draws a single measured series, and its header annotates the
period-level failure total instead — a real number from `byModel`. Do not
interpolate, estimate, or scale a second series from the period total: a
fabricated per-bucket error count would look authoritative and be wrong.
Recorded as a known gap in `plan.md`; the follow-up is roughly ten lines in those
two bucket functions and is **not** in this plan.

**2. The dimension tabs switch the donut, not the bar chart.** `/usage/chart` is
token and cost per time bucket, with no dimension breakdown, so no dimension can
filter the bars. The `byModel`/`byAccount`/`byApiKey`/`byEndpoint` maps do have
every dimension, so the tabs drive the donut. Label the strip so this is
unambiguous (e.g. "Cơ cấu theo"), rather than leaving a control whose effect a
reader has to infer from watching the wrong half of the panel change.

**3. The period strip stays in the page header, not here.** The reference puts
its time-range strip in the chart panel's header. Here one period drives every
panel — hero, KPIs, three tables — so a range control sitting inside the chart's
header would read as chart-local while silently reloading the whole page. It
stays in `PageHeader`'s `action` slot, where `UsageView.tsx:9` already has it, and
phase 06 turns the `<select>` there into the same segmented control this panel
uses for dimensions. Deviation from the reference, taken deliberately and stated
on screen by the panel header's own wording.

**4. Two metrics, one toggle, two consumers.** `tokens` and `cost` exist on both
`ChartPoint` and `PeriodSummary`, so the toggle can drive the bars and the donut
together. `cost` is `realCost` where present and `cost` otherwise (see phase 02),
because `realCost` is the pricing-derived figure and `cost` is the upstream's own
credit number — mixing them silently would be the same class of bug as the
period windows.

**5. A donut needs a centre, and recharts will not draw one.** Render the total
as an absolutely positioned HTML overlay inside a `relative` wrapper around the
`ResponsiveContainer`; do not reach for a custom SVG label. The overlay is
selectable text, which also makes it assertable in a test.

**6. `ResponsiveContainer` measures zero in jsdom.** A chart assertion cannot be
about rendered bars. Assert on the panel's own text: the toggle's state, the
donut's centre total, the legend entries, the error annotation, and the empty
state. Keep chart-shape assertions out of the unit test; they belong to phase
07's live check. The existing `UsageView.tsx:11` avoids this by only rendering
the chart when `chart.length` is non-zero — keep that guard, since it also
removes a console error.

**7. `Legend` is worth keeping for the donut.** With four dimensions and up to a
dozen slices, a colour key the reader cannot resolve is decoration. Cap the
slices (top 8 plus an "Khác" aggregate) so the legend stays readable, and let the
aggregate carry the remainder's value sum — never drop slices silently, because
then the centre total would not equal the legend.

## Requirements

**Functional**
- Header: a dimension tab strip (Model / Tài khoản / API key / Endpoint) and a
  metric toggle (Token / Chi phí), each a button group with `aria-pressed`.
- Body: a bar chart of `chart` for the selected metric, with the bucket label on
  the x-axis and a compact-number y-axis, plus a tooltip showing the exact value
  (`exactNumber` for tokens, four decimals for USD).
- Beside it: a donut of `shareByDimension(usage, dimension, metric)` with the
  total in its centre and a legend of the top 8 slices plus "Khác".
- A caption stating the period's failure total from `periodErrors`, and a second
  caption stating that the chart is a single series because the server publishes
  no per-bucket request or error counts.
- Empty states: no chart points, and an empty share list, each with its own
  wording.

**Non-functional**
- Chart-local state only (`dimension`, `metric`); the period arrives as a prop.
- No new dependency.
- Under 200 lines.
- No `NaN` reaches an SVG attribute or a CSS value.

## Architecture

```
UsageChartPanel
  props: { usage: UsageStats | null; chart: ChartPoint[]; period: string }
  state: dimension: UsageDimension = 'model'; metric: UsageMetric = 'tokens'
  ├── header  — dimension tabs (state) + metric toggle (state)
  ├── BarChart     ← chart[i].tokens | chart[i].cost
  └── donut        ← shareByDimension(usage, dimension, metric)
```

## Related code files

- **Create:** `web-next/src/components/UsageChartPanel.tsx`
- **Create:** `web-next/src/components/UsageChartPanel.test.tsx`
- **Modify:** none
- **Delete:** none

## Implementation steps

### Step 1: Write the failing test

`UsageChartPanel.test.tsx` asserts:

```
- renders the dimension tab strip with all four labels and marks 'Model' pressed
  by default
- clicking a dimension tab marks it pressed and unmarks the previous one, and the
  donut's legend then names that dimension's keys (a fixture whose byModel and
  byAccount keys differ makes this unambiguous)
- renders the metric toggle and marks 'Token' pressed by default
- switching to 'Chi phí' changes the centre total to the cost figure (fixture
  values chosen so the token total and the cost total cannot be confused)
- the donut's centre total equals the sum of the rendered slices
- caps the legend at eight slices plus 'Khác', and the centre total still equals
  the sum of all slices including the aggregate
- renders the period failure count from byModel errors
- renders the empty state when chart.length === 0, and does not render a
  BarChart then
- renders the empty state when no dimension has a positive value, with wording
  that does not claim the period was empty overall
- never renders 'NaN' anywhere in the panel's textContent for a zero-total usage
```

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- UsageChartPanel.test.tsx 2>&1 | tail -15
```

Expected: fails to resolve `./UsageChartPanel`.

### Step 3: Write the component

Header controls follow `ProvidersView.tsx:59`'s button group. Vietnamese labels:
"Model", "Tài khoản", "API key", "Endpoint"; "Token", "Chi phí"; "Cơ cấu theo";
"Lỗi trong kỳ"; the caption "Biểu đồ chỉ có một chuỗi: máy chủ không trả số
request hoặc lỗi theo từng bucket."; "Chưa có dữ liệu trong kỳ." for no points;
"Kỳ này chưa có lưu lượng để tính cơ cấu." for an empty share list.

The donut uses `<Pie innerRadius={...} outerRadius={...} dataKey="value" />`
inside `<PieChart>`, one `<Cell>` per slice, wrapped in a `relative` div with the
centre total as an absolutely positioned overlay carrying
`data-testid="donut-total"`.

Guard every arithmetic result before it reaches the DOM: a slice whose value is
not finite is filtered out by phase 02, so the component's remaining duty is not
to divide by a zero total — the centre total renders `0` rather than `$NaN`.

### Step 4: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- UsageChartPanel.test.tsx 2>&1 | tail -25
```

### Step 5: Full suite, typecheck, lint

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -12 && npx tsc -b && npm run lint 2>&1 | tail -10
```

### Step 6: Confirm the line budget and the dependency freeze

```bash
cd /Users/van/Tools/OmniProxy/web-next
wc -l src/components/UsageChartPanel.tsx && git diff --stat package.json package-lock.json
```

Expected: under 200 lines, and **no diff** on `package.json` / `package-lock.json`
— a chart library must not have been added.

### Step 7: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/components/UsageChartPanel.tsx web-next/src/components/UsageChartPanel.test.tsx
git commit -F - <<'EOF'
feat(web-next): rebuild the usage chart as a bar series plus a share donut

The trend was a single area chart with no controls. The panel now carries a
dimension strip and a metric toggle, drawing a bar per bucket and, beside it, the
share of the selected dimension with the period total in the donut's centre.

The reference's stacked success/error bars are not drawn: ChartDataPoint carries
only label, tokens and cost, and neither bucket function counts requests or
errors, so no per-bucket failure series exists for any range. The period's
failure count from byModel is annotated in the header instead of being spread
across buckets that never measured it.

The dimension tabs drive the donut only. /usage/chart has no dimension
breakdown, so nothing else in the panel can honour them.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `UsageChartPanel.test.tsx`
- [ ] Step 2: confirm the red
- [ ] Step 3: write the component
- [ ] Step 4: tests green
- [ ] Step 5: suite, `tsc -b`, oxlint clean
- [ ] Step 6: under 200 lines, no dependency added
- [ ] Step 7: commit

## Success criteria

- The stacked error series is absent and the panel says why.
- The dimension tabs change the donut's legend, and the metric toggle changes the
  centre total.
- Centre total equals the sum of the drawn slices, aggregate included.
- Empty chart and empty share each render their own wording.
- No `NaN` in any rendered text or attribute for a zero-total usage.
- `package.json` unchanged.

## Risk assessment

| Risk | Mitigation |
|---|---|
| A fabricated per-bucket error series (scaled from the period total) reads as measured | The panel draws one series and states the reason on screen; phase 07's check looks for a single `Bar` |
| Dimension tabs appear to filter the bar chart | Strip is labelled as the breakdown/donut control; the caption restates that the chart has one series |
| Centre total ≠ legend because slices were dropped | Phase 02 filters `value > 0` and totals the drawn slices; a test pins total == sum |
| `NaN` in an SVG attribute from a zero total | Zero-total test asserts no `NaN` in the panel's text; phase 02 guards the arithmetic |
| `ResponsiveContainer` measures 0 in jsdom, so the test asserts nothing real | Assertions are on text, `aria-pressed` and the centre overlay; shape assertions are deferred to phase 07 |
| Colour-only legend is unreadable for 30 models | Top 8 plus "Khác" aggregate, names shown as legend text |
| A chart library gets added | Step 6 asserts no `package.json` diff |

## Security considerations

- **No fetch, no credentials.** Props in, DOM out; the module imports types only.
- **Upstream model names are rendered as text** — React escapes them. A model ID
  can be arbitrary upstream data, so it is never placed in an attribute used as
  a selector.
- **No `dangerouslySetInnerHTML`**, including for the donut's centre label.
- **Tooltips reveal nothing new.** Every value shown is already in the panel's
  own summary; no credential, key material or account email is introduced by the
  tooltip that the panel did not already carry.

## Next steps

Phase 06 mounts the panel and turns the header's `<select>` into the same
segmented control, so the period strip and this panel's tabs read as one control
family.
