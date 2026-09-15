# Usage Dashboard Rework

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the admin client's "Sử dụng" page (`/admin-next/#usage`) so it
reads like the reference dashboard — a hero number, a cost panel, four KPI cards,
a chart panel holding a bar chart and a share donut, and three tables — using
only fields the existing admin API already returns.

**Architecture:** One Go fix and a frontend rebuilt from six small components over
one new pure-function module. The Go fix removes the `1h` period that three
separate switches each resolved to a different window. Every number on screen is
a field `/usage/stats`, `/usage/chart` or `/status` already sends, or a
documented client-side aggregate of one — no new endpoint, dependency or field.

**Tech stack:** Go 1.x (`proxy/`), React 19 + TypeScript + Vite 8 + Tailwind 4 +
recharts 3.10.1 + vitest 5 (`web-next/`). All already installed — add nothing.

## Global constraints

- **KISS / YAGNI / DRY.** Update the existing files directly; never create a
  parallel "enhanced" copy of one.
- **Real implementations only.** No mocks, no fake data, no panel invented just
  to fill a grid slot.
- **UI strings Vietnamese and hardcoded; code comments English.**
- **`.tsx` components stay PascalCase** (repo convention, e.g.
  `ProvidersView.tsx`); `src/lib/` files stay lowercase (`usage.ts`).
- **No file over 200 lines** (`development-rules.md`). Split, never truncate a
  panel to fit.
- **Never modify `web/`** — the legacy vanilla client at `/admin/`.
- **`Empty` is not shared.** It is private inside `UsageView.tsx:14` and
  `Overview.tsx:30`. Only `Card`/`PageHeader` come from `Shell.tsx:32-33`, and
  only `compactNumber`/`exactNumber`/`relativeTime` from `lib/format.ts`.
- **Lint before commit, tests before push, never ignore a red test.**
- **Commit convention:** conventional commits, no AI reference in the body; the
  `Co-Authored-By: Claude Code <noreply@anthropic.com>` trailer only.

## Phases

| # | Phase | Files | Status |
|---|---|---|---|
| 1 | [Fix the usage period window](phase-01-fix-usage-period-window.md) | `proxy/usage_tracker.go`, `proxy/usage_period_test.go` | Done — `cb14136` |
| 2 | [Usage derivations](phase-02-usage-derivations.md) | `web-next/src/lib/usage.ts`, `usage.test.ts` | Done — `3ad54c6` |
| 3 | [Hero and KPI row](phase-03-hero-and-kpi-row.md) | `UsageHero.tsx`, `UsageKpiRow.tsx` + tests | Done — `5518f0e` |
| 4 | [Chart panel](phase-04-chart-panel.md) | `UsageChartPanel.tsx` + test | Done — `e5697e0`, fixed by `c1ad145` |
| 5 | [Detail tables](phase-05-detail-tables.md) | `UsageModelsTable.tsx`, `UsageProvidersPanel.tsx`, `UsageRequestsTable.tsx` + tests | Done — `d8f62a7` |
| 6 | [Compose and live refresh](phase-06-compose-and-live-refresh.md) | `UsageView.tsx` + test, `App.tsx`, `ProvidersView.tsx` | Done — `f915574` |
| 7 | [Build, embed and verify live](phase-07-build-and-verify-live.md) | `webnext/dist/` | Done — `9afdfc2`; step 7 (browser) needs a human |

Phase 1 is independent of 2–7; phases 3, 4 and 5 each depend on 2 and are
independent of each other; phase 6 depends on 3–5; phase 7 depends on all.

## Why this order

Phase 1 first: it is the only backend change and it decides what the UI may
offer, so doing it last would leave the UI's period list asserting something
untrue. Phase 2 before 3–5 because the derivations are where the wrong-number
bugs live (a `0` read as missing, a share computed against a zero total, an
aggregate attributed by the coarse `provider` label) and they are testable
without rendering, which leaves phases 3–5 nothing to decide.

## Known gap (recorded, not fixed here)

`ChartDataPoint` (`proxy/usage_tracker.go:163-167`) exposes only `label`,
`tokens` and `cost`, so the reference's stacked success/error bar chart cannot be
drawn for any range: there is no per-bucket `requests` or `errors`. Phase 4
charts a single measured series instead. Adding `Requests int` and `Errors int`
to `ChartDataPoint` and incrementing both in `bucketByHour` and `bucketByDay` is
roughly ten lines in the same two functions — explicitly **not** in this plan.
`phase-02-usage-derivations.md` maps every on-screen number to its exact API
field, including the four the API cannot source (unbilled amount, per-row call
count, per-row key, a per-bucket error series) and what replaces each.

## Out of scope

Trend deltas (no baseline is kept), the `today | all | 60d` periods
(backend-supported, unexposed — see phase 01), and any SSE live mode:
`broadcastStats` marshals `GetStats("24h")` unconditionally
(`proxy/usage_tracker.go:891-897`), so the stream cannot honour a selector.
