# Phase 05 — Detail tables

**Priority:** High — the reference's three tables, and where the "cannot be
sourced" decisions become visible.
**Status:** Not started
**Depends on:** phase 02 (`modelRows`, `recentWindow`, `requestRows`,
`RING_CAPACITY`)
**Blocks:** phase 06

## Context links

- `web-next/src/components/UsageView.tsx:12` — the "Model theo lưu lượng" table
  being replaced, and its `data-table` usage
- `web-next/src/components/ProvidersView.tsx:108-124` — the ring-buffer
  disclosure this phase repeats, and the last time this repo had to explain that
  a panel is not a window
- `web-next/src/components/ProvidersView.tsx:126-130` — the `KV` falsy-trap
  comment; the same trap applies to `errors: 0`
- `web-next/src/lib/api.ts:57-60` — `RecentRequest`, including the note that
  `provider` is the coarse family
- `web-next/src/lib/format.ts:26-35` — `relativeTime`
- `web-next/src/lib/format.ts:14-24` — `compactNumber` / `exactNumber`
- `web-next/src/components/AccountsTable.tsx` — the repo's only existing
  table-heavy component, for idiom only; it uses `@tanstack/react-table`, which
  these three tables do **not** need (see Key insight 3)

## Overview

Three presentational components: `UsageModelsTable.tsx`,
`UsageProvidersPanel.tsx`, `UsageRequestsTable.tsx`. Each takes derived rows as
props and renders a `Card` with a `data-table` body.

## Key insights

**1. The "providers — last 5 minutes" panel is a client-side window over the
ring, and its limits go on the panel.** The honest construction:
`recentWindow(usage, 5, now)` filters `usage.recentRequests` to the last five
minutes and groups by `accountId`. Stated limits, all of which the panel
discloses in one line:
- the ring holds at most `RING_CAPACITY` (500) records, so under load the window
  silently truncates to the newest 500 — the derivation returns `truncated` and
  the panel says so when it is true;
- the ring is in-memory, so after a restart the panel is empty until traffic
  resumes (the same disclosure `ProvidersView` already makes);
- records are appended on completion, and `getRecentRequestsLocked`
  (`usage_tracker.go:709-742`) reverses the append order rather than sorting by
  timestamp, so a long-running request can appear after a newer one;
- a record whose timestamp is absent or unparseable is excluded rather than
  guessed at (`parseRecordTime` returns `null`).
Rows are labelled by credential (`accountName`, else `accountId.slice(0, 8)`),
never by `provider`.

**2. `/usage/providers` is deliberately not called.** It returns
`{providers:[{id,name}]}` and nothing else (`handler.go:12950-12980`): no counts,
no timestamps, and its `id`/`name` is the coarse routing family, so every
external vendor collapses into one row. Using it would produce a table that is
wrong about both the numbers and the entities. It is not added to `api.ts`.

**3. These tables do not need `@tanstack/react-table`.** That library earns its
keep in `AccountsTable.tsx` (442 lines: sorting, column visibility, virtualised
rows). Here each table is a fixed column set over at most a few hundred rows, and
`@tanstack/react-virtual` would add a scroll container that hides the first rows
from a reader scrolling the page. Plain `<table className="data-table">`, as
`UsageView.tsx:12` already does. YAGNI.

**4. `errors: 0` must render `"0"`, never `"—"`.** The same falsy trap
`ProvidersView` documents at length: a summary whose `errors` key is absent (a
server older than the field) and one that counts a genuine zero both arrive as
`undefined`/`0`, and `String(v || '—')` turns the legitimate zero into an em dash.
Phase 02 already maps absent to `0`; these components must render the number, and
`'—'` is reserved for an absent string, such as a null timestamp.

**5. The models table's "last call" column is a ring lookup, and its absence must
be visible.** `byModel` has no timestamp, so `lastCallAt` is the newest
`recentRequests` entry for that model. A model that served traffic three days ago
and none since has no entry and renders `'—'`, which is correct and is exactly
what the column means: "last call within the ring", not "last call ever". The
column header says so.

**6. The requests table drops its call-count column.** The ring stores one record
per request, so the column would read `1` on every row. A dropped column is
stated in the commit message and in the panel's caption so a reader comparing
against the reference knows it was a decision.

**7. Long model IDs and error strings must not break the layout.** Model IDs are
arbitrary upstream strings; error messages can be multi-line upstream text. Model
cells use `font-mono text-xs` with `title` for the full value and `truncate`;
error text wraps (`break-words`) rather than truncating, because a half-visible
error is the least useful thing on the page. This mirrors
`ProvidersView.tsx:121`'s `w-full break-words text-red-700`.

**8. Row keys.** Use the model name / `accountId` for stable keys where unique,
and `${accountId}-${index}` for the requests table — the ring can hold several
records with the same model and account, and a duplicated React key would drop
rows silently. `ProvidersView.tsx:116` already uses that composite form for the
same reason.

## Requirements

**Functional**
- Models table: rank, model, tokens (compact, exact in `title`), a share bar,
  error count, last call (relative, `'—'` when absent). Sorted by requests
  descending, capped at a named limit.
- Providers panel: one row per credential active in the last five minutes, with
  calls, errors, tokens and last call; the disclosure line from Key insight 1;
  the truncation note when `truncated`; a distinct empty state.
- Requests table: time (relative, `'—'` when absent), key, model, credential,
  result with the error text, total tokens, cost. Newest first, capped.
- Every table renders a distinct empty state; none renders a bare header.

**Non-functional**
- No fetch, no `api` import, no state.
- Each file under 200 lines.
- Reuse `data-table` (`UsageView.tsx:12`); add no stylesheet.
- Numbers use `exactNumber` for tabular figures and `compactNumber` where the
  column is a glance value.

## Architecture

```
UsageModelsTable    { usage }              → modelRows(usage, 12)
UsageProvidersPanel { usage; now }         → recentWindow(usage, 5, now)
UsageRequestsTable  { usage }              → requestRows(usage, 20)
```

`now` is a prop so a test pins the window instead of racing the clock.

## Related code files

- **Create:** `web-next/src/components/UsageModelsTable.tsx` + `.test.tsx`
- **Create:** `web-next/src/components/UsageProvidersPanel.tsx` + `.test.tsx`
- **Create:** `web-next/src/components/UsageRequestsTable.tsx` + `.test.tsx`
- **Modify:** none
- **Delete:** none

## Implementation steps

### Step 1: Write the models-table test

`UsageModelsTable.test.tsx` asserts:

```
- renders one row per model, ranked from 1 in requests-descending order
- the first row's share bar width is 100% and a model with half the requests is
  at 50% (fixture chosen so the arithmetic is exact)
- a summary without an errors key renders '0' and never '—'
- a model whose only recentRequests record lacks a timestamp renders '—' in the
  last-call cell rather than 'NaN'
- a model absent from recentRequests renders '—' in the last-call cell while
  still showing its tokens
- respects the 12-row cap
- renders the empty state for a usage with no models, with its own wording
- never renders 'NaN'
```

### Step 2: Run, then write `UsageModelsTable.tsx`

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- UsageModelsTable.test.tsx 2>&1 | tail -15
```

Headers: "Model", "Token", "Tỷ lệ", "Lỗi", "Lần gọi gần nhất". The share cell is
an inline `div` with a width percentage, matching
`Overview.tsx:23`'s existing bar idiom (`h-1.5 overflow-hidden rounded-full
bg-slate-100` with an inner `bg-blue-600`), so the page keeps one bar style.

### Step 3: Write the providers-panel test

`UsageProvidersPanel.test.tsx` asserts:

```
- includes a record 2 minutes old and excludes one 6 minutes old (now is a
  fixture constant, so the assertion is deterministic)
- groups two records from the same accountId into one row and keeps two different
  accounts apart
- labels a row with accountName, and falls back to the first 8 characters of
  accountId when it is absent
- never renders the coarse routing family ('External OpenAI') as a row label,
  even when every fixture record carries it as provider
- counts only in-window records in the calls and errors columns
- renders the truncation note when the ring holds RING_CAPACITY in-window
  records, and does not render it otherwise
- renders the restart disclosure line unconditionally
- renders the empty state when nothing is in the window
- excludes a record with an unparseable timestamp
```

### Step 4: Run, then write `UsageProvidersPanel.tsx`

```bash
npm test -- UsageProvidersPanel.test.tsx 2>&1 | tail -15
```

Headers: "Tài khoản", "Gọi", "Lỗi", "Token", "Lần gọi gần nhất". Disclosure,
verbatim: "Nguồn là bộ đệm 500 request gần nhất trong bộ nhớ, không phải một
cửa sổ thời gian đầy đủ — sau khi khởi động lại sẽ trống." Truncation note:
"Bộ đệm đã đầy: cửa sổ 5 phút có thể bị cắt." Empty state: "Chưa có request nào
trong 5 phút gần nhất."

### Step 5: Write the requests-table test

`UsageRequestsTable.test.tsx` asserts:

```
- renders newest first (fixture timestamps out of order in the ring, so passing
  proves the sort)
- token column is inputTokens + outputTokens
- a record without apiKeyId renders '—' in the key column, and one with it renders
  the value
- credential falls back to the first 8 characters of accountId
- a failed record renders its status and the error text; a successful record
  renders no error text
- cost renders as USD with four decimals, and a record without realCost renders
  '—'
- respects the 20-row cap
- renders the empty state for an empty ring
- never renders 'NaN' or 'undefined'
```

### Step 6: Run, then write `UsageRequestsTable.tsx`

```bash
npm test -- UsageRequestsTable.test.tsx 2>&1 | tail -15
```

Headers: "Thời gian", "API key", "Model", "Tài khoản", "Kết quả", "Token",
"Chi phí". There is deliberately no call-count column (Key insight 6); the caption
says a record is one request.

### Step 7: Full suite, typecheck, lint, line budget

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -12 && npx tsc -b && npm run lint 2>&1 | tail -10
wc -l src/components/UsageModelsTable.tsx src/components/UsageProvidersPanel.tsx src/components/UsageRequestsTable.tsx
```

Expected: everything green, each file under 200 lines.

### Step 8: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/components/UsageModelsTable.tsx web-next/src/components/UsageModelsTable.test.tsx web-next/src/components/UsageProvidersPanel.tsx web-next/src/components/UsageProvidersPanel.test.tsx web-next/src/components/UsageRequestsTable.tsx web-next/src/components/UsageRequestsTable.test.tsx
git commit -F - <<'EOF'
feat(web-next): add the usage detail tables

Three tables replace the single "Model theo lưu lượng" list: the models ranked by
share with errors and last call, the credentials active in the last five minutes,
and the recent requests with their key, credential, result and cost.

The five-minute panel is a client-side filter over the recent-requests ring, not
a call to /usage/providers: that route returns provider names and nothing else,
and its name is the coarse routing family, so every external vendor would collapse
into one row with no counts. Rows are attributed by accountId. The panel states
the ring's three limits rather than presenting the window as complete.

The requests table has no call-count column. The ring stores one record per
request, so the column could only ever read 1. A zero error count renders "0",
not an em dash.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `UsageModelsTable.test.tsx`
- [ ] Step 2: red, then write `UsageModelsTable.tsx`
- [ ] Step 3: write `UsageProvidersPanel.test.tsx`
- [ ] Step 4: red, then write `UsageProvidersPanel.tsx`
- [ ] Step 5: write `UsageRequestsTable.test.tsx`
- [ ] Step 6: red, then write `UsageRequestsTable.tsx`
- [ ] Step 7: suite, `tsc -b`, oxlint clean; files under 200 lines
- [ ] Step 8: commit

## Success criteria

- The five-minute panel's window is deterministic under a fixture `now`, and its
  truncation note appears exactly when the ring caps it.
- No provider row is labelled with a routing family.
- A zero error count renders `"0"`; a null timestamp renders `"—"`.
- The requests table sorts newest first even when the ring arrives unsorted.
- The requests table has no call-count column, and the panel says a record is one
  request.
- Every table has its own empty state.
- `@tanstack/react-table` is not imported by any of the three.

## Risk assessment

| Risk | Mitigation |
|---|---|
| The five-minute panel is read as a complete and durable window | Disclosure line names the 500-record buffer and the restart reset; `truncated` adds a second note; a test pins both |
| Rows attributed by the coarse `provider` label | Test asserts no row label equals `'External OpenAI'` while every fixture record carries it |
| A legitimate `0` errors cell renders `—` | Test pins the `errors`-absent case to `'0'` |
| Duplicate React keys silently drop rows | Composite `${accountId}-${index}` keys for the requests table |
| A model ID or upstream error breaks the layout | Models truncate with a `title`; errors wrap with `break-words` |
| `new Date()` inside a component makes the 5-minute test flaky | `now` is a prop; tests pass a constant |
| Virtualisation hides rows and breaks `getByText` | Plain tables, no virtualiser |
| Over 200 lines per file | Split each table's row renderer into a `…Row` component in the same file; if still over, one file per table already exists to move into |

## Security considerations

- **The `API key` column shows an identifier, never key material.** `apiKeyId` is
  `config.ApiKeyEntry.ID` (`proxy/auth.go:110-118`), and the legacy client already
  renders `byApiKey` keys under that label (`web/usage.js:1017-1022`). No masking
  is added or removed; nothing new is exposed.
- **Upstream error strings are rendered as text and can contain an email or a
  model ID.** React escapes them, and `ProvidersView`'s panel already shows the
  same class of string, so no new disclosure channel is opened.
- **Read-only.** No control on any table mutates state, fetches, or navigates.
- **No credential-shaped value in a `title` attribute.** `title` carries model IDs
  and counts only.
- **The panels cannot be read as accounting.** The ring's limits are disclosed, so
  a truncated five-minute window cannot be mistaken for measured traffic, and the
  effective-token card is not labelled as a billing figure.

## Next steps

Phase 06 composes the three tables under the hero, KPI row and chart panel, and
threads `now` from a single place so the five-minute panel re-evaluates on each
poll rather than freezing at mount.
