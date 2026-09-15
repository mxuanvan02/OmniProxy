# Phase 03 — Hero and KPI row

**Priority:** High — this is the block the reference leads with.
**Status:** Not started
**Depends on:** phase 02 (`heroStats`, `kpiCards`)
**Blocks:** phase 06

## Context links

- `web-next/src/components/UsageView.tsx:14` — the current local `Metric` card
  being replaced
- `web-next/src/components/Overview.tsx:28` — `Metric` with the `tone` union this
  phase reuses (`ok | warn | bad`)
- `web-next/src/components/Shell.tsx:32-33` — `PageHeader`, `Card`
- `web-next/src/lib/format.ts:14-24` — `compactNumber` vs `exactNumber`
- `web-next/src/lib/format.ts:26-35` — `relativeTime`
- `web-next/src/lib/api.ts:55` — `Status`
- `web-next/src/components/QuotaView.test.tsx` — the render-and-assert idiom

## Overview

Two presentational components. `UsageHero.tsx` renders the large success-token
figure with its sub-stats line and, beside it, the cost panel with a live
indicator. `UsageKpiRow.tsx` renders the four KPI cards. Neither fetches, neither
derives: both take `heroStats` / `kpiCards` output as props and own no state.

## Key insights

**1. `Card` does not forward arbitrary props.** `Shell.tsx:33` declares
`{children, className}` exactly, so no `data-testid` reaches it. Each testable
panel gets a wrapper `<div data-testid="…">`, as `ProvidersView` already does
(`ProvidersView.tsx:66`). Do not widen `Card`.

**2. Two formatters, two jobs.** `compactNumber` rounds to one decimal ("1.3B"
hides up to 50M) and exists for the size read at a glance; `exactNumber` is the
grouped figure for whatever must stay reachable. The hero uses both: the big
number is `compactNumber`, and `exactNumber` sits in a `title` attribute so the
precise value is still obtainable without a second row. This is the pattern
`format.ts:20-21` documents.

**3. There is no card-shaped hole to fill.** The reference's fourth KPI
("unbilled amount") has no source; phase 02 replaced it with effective tokens.
Do not add a fifth card to compensate, and do not relabel a sourced number to
sound like a billing one.

**4. The live indicator must not over-claim.** The page polls; it does not
stream. The indicator says "đang phục vụ" when
`usage.activeRequests.length > 0`, and the panel states the cadence rather than
looking like a socket. `App.tsx:132-138` is where the cadence comes from and
phase 06 extends it to this section.

**5. A `0` is not a missing value.** `Statistics.available` of `0` means the pool
is empty, which is the opposite of "no data", and `compactNumber(0)` already
returns `'0'`. Never render `'—'` for a numeric field; reserve `'—'` for a
genuinely absent string, like a `null` timestamp.

**6. Zero state is a real state.** A fresh deployment has `totalRequests === 0`
and `status === null`. Both components must render a coherent block — not `NaN`,
not `$NaN` — because `App.tsx:170` briefly renders the section before its first
load completes. Phase 02's tests pin the values; this phase's tests pin the
render.

## Requirements

**Functional**
- Hero: one dominant figure (`successTokens`, compact) with `exactNumber` in the
  `title`; a sub-stats line carrying total requests, failed requests, last call
  (`relativeTime`, `'—'` when null), active/total credentials, and model count.
- Cost panel beside it: `totalRealCost` as the headline USD figure, `totalCost`
  as the secondary credit figure, and a live indicator from
  `activeRequests > 0`, with a one-line cadence note.
- Four KPI cards in reference order, each with label, value, note and optional
  tone.
- Both panels render a coherent zero state.

**Non-functional**
- No fetch, no `api` import, no state.
- Each file under 200 lines.
- Props strictly typed; no `any`.

## Architecture

```
UsageView (phase 06)
  ├── UsageHero        { stats: HeroStats; updatedAt: number | null }
  └── UsageKpiRow      { cards: KpiCard[] }
```

`updatedAt` is `Shell`'s existing prop (`Shell.tsx:15`), already threaded from
`App.tsx:166`; reuse it rather than inventing a second clock.

## Related code files

- **Create:** `web-next/src/components/UsageHero.tsx`
- **Create:** `web-next/src/components/UsageHero.test.tsx`
- **Create:** `web-next/src/components/UsageKpiRow.tsx`
- **Create:** `web-next/src/components/UsageKpiRow.test.tsx`
- **Modify:** none — `UsageView.tsx` is rewritten in phase 06, in one piece
- **Delete:** none

## Implementation steps

### Step 1: Write the failing hero test

`UsageHero.test.tsx` asserts:

```
- renders the compact success-token figure and exposes the exact figure in the
  element's title attribute (one test: 1_530_000_000 renders '1.5B' with
  title '1.530.000.000')
- renders each sub-stat: total requests, failed requests, active/total
  credentials, model count
- renders '—' for a null lastCallAt, and never 'NaN'
- renders 'đang phục vụ' when activeRequests > 0 and the idle wording when 0
- renders a coherent zero state for heroStats(null-usage, null-status): no 'NaN'
  and no 'undefined' anywhere in the panel's textContent
- renders the headline cost as a USD figure from totalRealCost
```

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- UsageHero.test.tsx 2>&1 | tail -15
```

Expected: fails to resolve `./UsageHero`.

### Step 3: Write `UsageHero.tsx`

Structure: an outer `div` with `data-testid="usage-hero"`, a grid that stacks on
small screens and splits at `xl`, the hero `Card` on the left and the cost `Card`
on the right. Labels Vietnamese: "Token đầu ra", "Request", "Request lỗi",
"Lần gọi gần nhất", "Tài khoản", "Model", "Chi phí thực (USD)",
"Credit upstream báo cáo". Idle wording: "Chưa có request đang chạy". Serving
wording: "Đang phục vụ".

The `title` attribute carries `exactNumber(stats.successTokens)`. Wrap the figure
in a `span` that is `break-all`-safe at narrow widths so a long exact number
cannot force the page to scroll horizontally.

### Step 4: Write the failing KPI test

`UsageKpiRow.test.tsx` asserts:

```
- renders exactly four cards (query by a shared data-testid prefix and count)
- renders each card's label and value, using compactNumber for the big ones
- the fourth card's label names effective tokens and is not a billing claim
- no card's text contains 'unbilled', 'chưa xuất hoá đơn', or a 'NaN'
- a card carrying tone 'bad' applies the red text class and a card with no tone
  does not
- renders the four cards for a zero-state input rather than nothing
```

### Step 5: Run to verify it fails, then write `UsageKpiRow.tsx`

```bash
npm test -- UsageKpiRow.test.tsx 2>&1 | tail -15
```

`UsageKpiRow.tsx` maps `cards` to a grid of `Card`s, one `data-testid` per
`card.key` so a test addresses a card by name. Tone classes mirror
`Overview.tsx:28` exactly (`text-red-700` / `text-amber-700` / `text-emerald-700`
/ `text-slate-950`) — reusing the established palette rather than introducing a
second one. Every card's note is non-empty, because a KPI nobody can qualify is a
KPI nobody can act on.

### Step 6: Run both suites green, then typecheck and lint

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -12 && npx tsc -b && npm run lint 2>&1 | tail -10
```

### Step 7: Confirm the line budget

```bash
cd /Users/van/Tools/OmniProxy/web-next && wc -l src/components/UsageHero.tsx src/components/UsageKpiRow.tsx
```

Expected: each under 200.

### Step 8: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/components/UsageHero.tsx web-next/src/components/UsageHero.test.tsx web-next/src/components/UsageKpiRow.tsx web-next/src/components/UsageKpiRow.test.tsx
git commit -F - <<'EOF'
feat(web-next): lead the usage page with a hero figure and a KPI row

The page opened on four equal-weight metric cards, so nothing was the headline.
The success-token total is now the large figure, with the exact value reachable
through the title attribute rather than a second row, and the period's request,
failure, credential and model counts sit underneath it. Beside it a cost panel
reports the pricing-derived USD figure and the upstream-reported credits
separately, because they are different measurements.

The live indicator reads activeRequests and states the poll cadence instead of
implying a stream. The fourth KPI is effective tokens: nothing in the API
separates billed from unbilled work, so the reference's "unbilled amount" is
dropped rather than mislabelled.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `UsageHero.test.tsx`
- [ ] Step 2: confirm the red
- [ ] Step 3: write `UsageHero.tsx`
- [ ] Step 4: write `UsageKpiRow.test.tsx`
- [ ] Step 5: write `UsageKpiRow.tsx`
- [ ] Step 6: suite, `tsc -b`, oxlint clean
- [ ] Step 7: files under 200 lines
- [ ] Step 8: commit

## Success criteria

- The hero shows a compact figure whose `title` carries the exact value.
- A null `lastCallAt` renders `'—'`; an in-flight request renders the serving
  wording.
- Zero-state input renders no `NaN` and no `undefined`.
- Exactly four KPI cards, and none of them claims to be a billing figure.
- No `data-testid` was attached to `Card` (which cannot forward it).

## Risk assessment

| Risk | Mitigation |
|---|---|
| `Card` receives a `data-testid` that silently does nothing | Wrapper `div`s carry every testid; the tests query those, so a broken one fails loudly |
| `$NaN` in the cost panel on a fresh deployment | Zero-state test asserts no `NaN` in `textContent`, for both components |
| `compactNumber`'s rounding hides the exact figure | `title` holds `exactNumber`; a test pins the pair |
| The live indicator reads as a websocket | Wording is "đang phục vụ" + a cadence note; no animated dot implying a socket |
| A padding fifth card sneaks in to "match the reference" | Test counts cards |
| A long exact token number forces horizontal page scroll | The figure is in a `break-all`-safe span; the whole page is already `mx-auto max-w-7xl` inside `overflow-auto` |

## Security considerations

- **Read-only and credential-free.** No `api` import, no state, no user input.
- **No `dangerouslySetInnerHTML`.** Every value is a number or a fixed Vietnamese
  string; the one externally-sourced string (`Status` is server data) is rendered
  as text.
- **No new data leaves the browser.** Nothing here calls a route.
- **The indicator cannot be mistaken for durable state.** It reflects
  `activeRequests` right now, which is empty after a restart, and the panel says
  so, mirroring the honesty rule `ProvidersView`'s pool panel already follows.

## Next steps

Phase 06 renders both components from `App.tsx`'s existing `usage` state and the
`updatedAt` it already tracks, and extends the poll interval to this section so
the indicator is truthful rather than decorative.
