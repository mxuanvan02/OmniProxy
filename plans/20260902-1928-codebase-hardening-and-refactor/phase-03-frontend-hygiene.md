# Phase 03 — Frontend consolidation & repo hygiene

**Context:** [plan.md](plan.md) · source: tests/CI/frontend review (MED 11, LOW 12, LOW 14)
**Priority:** MED · **Status:** ✅ complete (runtime click-through pending) · **Parallel group:** A (concurrent with 01, 02)

## Overview

Three small, independent cleanups: `escapeHtml` is defined twice and consumed via implicit script-scope globals; `web/icon.svg` is a 2 MB raster-in-SVG that nothing references; two locale files each miss one key.

## Key insights

- `escapeHtml` exists at `web/app.js:22` and `web/quota.js:85`, attached to neither `window` nor a module. `accounts.js` and `usage.js` call it without defining it — this works only because classic-script top-level `function` declarations become globals, so it is **load-order dependent**. Not a live bug; a fragile one.
- `web/icon.svg` (2,080,689 B) is an Inkscape file wrapping a single base64 PNG (0 `<path>` elements). `index.html` references `/admin/favicon.ico` (lines 8, 45, 99, 679); `styles.css` never references the SVG. Not served on page load → deleting it costs nothing at runtime.
- Locales: en 1020 keys, vi 1020, zh 1019. `kirocli.browse` missing from **both** vi and zh; vi carries one key absent from en.
- **Do not touch `web/app.js` auth code** — phase 04 rewrites all three JS auth call sites (`app.js`, `logs.js`, `usage.js`). This phase only moves `escapeHtml` out of `app.js`.
- **Do not delete `_backups/refactor-wip-20260901-111329/`** — phase 07 must diff against it. Hygiene there is deferred to 07.

## Requirements

Functional
- One definition of `escapeHtml` (and `escapeAttr` if likewise duplicated), loaded before every consumer.
- Dashboard renders identically after the move (no regression in accounts / usage / quota views).
- vi + zh locales complete relative to en; vi's orphan key removed.

Non-functional
- No build step introduced. `web/` stays classic scripts loaded from `index.html`.

## Architecture

```
index.html
  <script src="escape.js">   ← new, first
  <script src="app.js">      ← escapeHtml removed
  <script src="quota.js">    ← escapeHtml removed
  <script src="accounts.js"> ← unchanged (was already a consumer)
  <script src="usage.js">    ← unchanged
```

Attach explicitly to `window` in `escape.js` so the dependency is declared rather than incidental.

## Related code files

Create
- `web/escape.js`

Modify
- `web/index.html` — add the `escape.js` tag before all consumers
- `web/app.js` — remove local `escapeHtml` (line ~22) only; leave auth code alone
- `web/quota.js` — remove local `escapeHtml` (line ~85)
- `web/locales/vi.json` — add `kirocli.browse`, remove the orphan key
- `web/locales/zh.json` — add `kirocli.browse`

Delete
- `web/icon.svg` (2 MB, unreferenced)

## Implementation steps

1. Grep for every `escapeHtml` / `escapeAttr` definition and call site across `web/*.js` — confirm the two definitions are byte-identical before consolidating; if they differ, keep the stricter one and note why.
2. Create `web/escape.js` with the shared implementation(s), assigning to `window.escapeHtml` (and `window.escapeAttr`) as well as leaving the bare function declaration, so existing unqualified call sites keep working.
3. Remove the two local definitions.
4. Add the `<script>` tag to `index.html` ahead of `app.js`.
5. Locales: diff en against vi and zh key-by-key; add `kirocli.browse` to both with correct Vietnamese / Chinese text; delete vi's extra key. Verify all three files stay valid JSON and end at 1020 keys.
6. `git rm web/icon.svg`. Re-grep the whole repo (including `resources/`, Go embeds) for `icon.svg` to prove nothing loads it before deleting.
7. Load the dashboard and click through accounts / usage / quota / logs tabs to confirm no `escapeHtml is not defined`.

## Todo

- [x] Both `escapeHtml` definitions diffed — **NOT identical**; kept stricter (quota.js char class) + safer null check (app.js)
- [x] `web/escape.js` created, exports on `window`
- [x] Duplicates removed from `app.js`, `quota.js`
- [x] `index.html` loads `escape.js` first (parser-blocking, before the async loader)
- [x] `kirocli.browse` added to vi + zh
- [x] vi orphan `apiKeys.title` removed; all 3 locales 1020 keys, valid JSON, identical key sets
- [x] `icon.svg` proven unreferenced (only self-ref in its own `sodipodi:docname`), deleted
- [ ] Dashboard tabs render clean, no console errors — **UNVERIFIED (no browser); static load-order + syntax checks only**

## Success criteria

- `grep -c 'function escapeHtml' web/*.js` = 1.
- All three locale files parse and have identical key sets.
- Repo drops ~2 MB; no runtime reference to `icon.svg` anywhere.
- No new console errors on any dashboard tab.

## Risk assessment

| Risk | Mitigation |
|------|-----------|
| A consumer loads before `escape.js` → `undefined` | Place the tag first in `index.html`; click through every tab |
| The two definitions differ subtly (one escapes quotes, the other not) → XSS regression | Step 1 diffs them explicitly; keep the stricter version |
| `icon.svg` is embedded via Go `embed` rather than HTML | Step 6 greps Go sources too before deleting |
| Merge conflict with phase 04 in `app.js` | 03 touches only the `escapeHtml` block; 04 owns the auth block |

## Security considerations

- Consolidating escaping is a security-positive change **only if** the surviving implementation is the stricter one — verify it escapes `& < > " '`.
- The frontend review inspected ~15 of ~120 `innerHTML` sites and found no unescaped interpolation; `quota.js`'s 11 sites remain unverified. Phase 04 adds CSP as the backstop. Do not treat this phase as an XSS audit.

## Next steps

Phase 04 (security) takes over `web/app.js` / `logs.js` / `usage.js` for the session-token migration.
