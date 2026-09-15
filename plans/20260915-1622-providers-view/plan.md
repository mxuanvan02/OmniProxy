# Providers View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-vendor "Nhà cung cấp" page to the React admin client at `/admin-next/`, showing each upstream's API link, call counts, error counts and live pool health.

**Architecture:** Two small Go additions back one new React section. `PeriodSummary.Errors` makes error counts windowable; a new `AccountPool.HealthSnapshot()` exposes the cooldowns, per-model locks and failure reasons that already exist in memory but have no HTTP route. The frontend derives every vendor aggregate client-side from three existing endpoints plus the new one — no aggregation is added server-side beyond the health snapshot.

**Tech Stack:** Go 1.x (`proxy/`, `pool/`), React 19 + TypeScript + Vite + Tailwind 4 + vitest (`web-next/`).

**Spec:** `plans/20260915-1622-providers-view/design.md`

## Global Constraints

- **Never modify `web/`** (the legacy vanilla client served at `/admin/`). It stays untouched.
- **All user-facing strings are Vietnamese**, hardcoded. `web-next` has no i18n layer.
- **Code comments are English**, matching the existing files.
- **`Section`, `KV`, `Label` are file-local to `AccountsView.tsx`** and are not exported. Do not export them, and do not edit `AccountsView.tsx`.
- **Do not use the CSS classes `.chip`, `.chip-on`, `.btn-danger-soft`** — they are referenced in `AccountsView.tsx:32,42` but defined in no stylesheet. Use `.btn`, `.btn-primary`, `.input`, or inline Tailwind.
- **Never call `config.*` while holding `p.mu`** in `pool/account.go` (lock order documented at `pool/account.go:322-326` and `:1029-1030`).
- **Sandbox note:** Go builds and tests in this repo need `GOCACHE="$TMPDIR/gocache"`. `TestSearchAdaptersUseNativeContracts/jina-reader` fails on DNS in the sandbox — a pre-existing failure unrelated to this work.
- **Commit convention:** conventional commits, no AI references in the body; the `Co-Authored-By: Claude Code <noreply@anthropic.com>` trailer only.

---

## Verified vendor inventory

Counted from the live `data/config.json` (66 accounts) while writing this plan.
The grouping rule has to survive this exact data:

| `baseUrl` | accounts | note |
|---|---|---|
| `https://gorouter.app/` | 13 | |
| `https://tabitoken.com/` | 10 | |
| *(empty)* | 8 | 7 provider groups — see below |
| `https://kiro.pix4k.com/` + `https://kiro.pix4k.com` | 4 + 2 | **splits without normalisation** |
| `https://api.xpiki.com` | 5 | |
| `https://token.vietshare.site/cdx/v1` | 5 | only baseUrl carrying a path |
| `https://www.sotamodel.net` + `https://www.sotamodel.net/` | 2 + 1 | **splits** |
| `https://fxqidian.de5.net/` + `https://fxqidian.de5.net` | 2 + 1 | **splits** |
| `https://api.justwoker.icu` + `https://api.justwoker.icu/` | 1 + 1 | **splits** |
| `https://agentrouter.org` | 2 | |
| `7e8e0296acbc.nofx.one`, `api.aeramc.su`, `api.apiforcode.com`, `api.hcnsec.cn`, `apikey.click`, `emtf.aipm9527.xyz`, `seekai.cc`, `vsllm.com`, `vyceai.com` | 1 each | 9 hosts |

That is **18 distinct hosts** and **7 provider groups**, so the page must render
**25 cards**. The four pairs marked *splits* are the whole reason `hostOf`
exists: without it the same count comes out **29**, and each half of
`kiro.pix4k.com` reports its own error rate against its own 4 or 2 accounts.

The 7 provider groups, with the `provider` values as stored:

| `provider` | accounts | `providerKind` |
|---|---|---|
| `Google Antigravity` | 2 | *(empty)* |
| `OpenAI Codex` | 1 | *(empty)* |
| `Gommo AutoAI` | 1 | `image` |
| `tavily`, `jina-reader`, `firecrawl`, `exa` | 1 each | `search` |

Two facts this establishes:

1. **The trailing slash is inconsistent per vendor**, so normalisation is
   mandatory.
2. **The 8 accounts with no `baseUrl` are not only Kiro/Codex/Antigravity.**
   Four of the seven groups serve search rather than chat, and Gommo's kind is
   `image`. They still belong on the page: hiding them would make its totals
   disagree with `/admin/api/accounts`. `provider` is set for all eight, so the
   `provider → providerKind → authMethod → 'Khác'` fallback never reaches its
   last arm on the current data.

---

## Phases

| # | Phase | Files | Status |
|---|---|---|---|
| 1 | [Windowed error counts](phase-01-windowed-error-counts.md) | `proxy/usage_tracker.go` | Not started |
| 2 | [Pool health snapshot + route](phase-02-pool-health-snapshot.md) | `pool/account.go`, `pool/account_model_selection_test.go`, `proxy/handler.go` | Not started |
| 3 | [Frontend data layer](phase-03-frontend-data-layer.md) | `web-next/src/lib/{api,providers}.ts` | Not started |
| 4 | [Providers view](phase-04-providers-view.md) | `web-next/src/components/ProvidersView.tsx` | Not started |
| 5 | [Wire navigation](phase-05-wire-navigation.md) | `web-next/src/components/Shell.tsx`, `web-next/src/App.tsx` | Not started |
| 6 | [Build and verify live](phase-06-build-and-verify.md) | `webnext/dist/` | Not started |

Phases 1 and 2 are independent of each other and of 3–5. Phase 3 depends on both
(its TypeScript types mirror both response shapes). Phase 4 depends on 3. Phase 5
depends on 4. Phase 6 depends on all.

## Why this order

Phase 1 before phase 2 because it is three lines and carries the one regression
that would silently produce a wrong page (see its phase file). Phase 3 before 4
because the pure functions are where the vendor-grouping bugs live, and they are
testable without rendering — writing them first means phase 4 has nothing to
decide.

## Out of scope

Per-vendor model ID lists, per-vendor quota rollup, per-vendor latency,
persisting cooldowns across restarts, `#providers/<vendor>` sub-pages. See
design.md §6 for why each is deferred.
