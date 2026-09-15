# Phase 04 — Providers view

**Priority:** Critical — this is the deliverable the user asked for.
**Status:** Not started
**Depends on:** phase 03 (its `VendorRow` / `RecentErrorRow` shapes)
**Blocks:** phase 05

## Context links

- `plans/20260915-1622-providers-view/design.md` §5 (conventions), §6 (v1 scope)
- `web-next/src/components/QuotaView.tsx` — the card-grid idiom this follows
- `web-next/src/components/AccountsView.tsx:51-53` — `Label`/`Section`/`KV`, file-local
- `web-next/src/components/QuotaView.test.tsx` — the test idiom
- `web-next/src/lib/format.ts`, `web-next/src/index.css`

## Overview

One props-in view. It fetches nothing on mount: `App.tsx` owns the data, as it
does for every other section. This phase renders `VendorRow[]` and
`RecentErrorRow[]` and adds no derivation of its own.

## Key insights

**1. `Card` does not forward arbitrary props.** `Shell.tsx:32` declares
`{children, className}` exactly, so a `data-testid` cannot reach it. Each vendor
card gets a wrapper `<div data-testid="vendor-<key>">` and that div is the grid
child. The alternative — widening `Card` to `{...rest}` — edits `Shell.tsx` for
an unrelated reason and collides with phase 05's edits to the same file.

**2. The `KV` falsy trap is the one rendering bug that matters.** `AccountsView`'s
`KV` is `String(v||'—')`, so a legitimate `0` renders as an em dash. On a page
whose whole job is reporting that a vendor has zero errors, that reads as "no
data" instead of "clean". The local copy must test for null and empty string
only.

**3. The health prop cannot be called `health`.** `format.ts` exports a function
named `health(a: Account)`, which this view needs for the roster breakdown. The
prop is `pool`.

**4. Every account in a vendor group shares one `baseUrl`.** They group on it, so
`row.accounts[0].baseUrl` is the vendor's API link. A provider-grouped row
(Codex, Antigravity, the four search services) has none, so the link line is
conditional and the card falls back to the account roster.

**5. `relativeTime(NaN)` is already safe.** `format.ts:27` returns `'—'` for any
falsy input, and `Date.parse('')` is `NaN`. A recent-request record with no
timestamp renders `'—'` rather than "NaN ngày trước".

**6. `.chip` and `.btn-danger-soft` do not exist.** `index.css` defines `.btn`,
`.btn-primary`, `.input` and nothing else. The period selector is therefore a
segmented control built from inline Tailwind, not `.chip`.

## Requirements

**Functional**
- One card per vendor: label, API link, calls (period + lifetime), errors
  (period + lifetime), error rate, tokens and cost for the period, model count
  with its catalog state and the catalog error verbatim when there is one, and
  the account roster by health.
- A vendor currently refusing traffic is marked `Đang bị chặn`, with the
  cooldown deadline, the reason, and the locked models; one strike from a lock is
  marked `Sắp bị chặn`.
- Period selector over `24h | 7d | 30d`, driving the parent's `usagePeriod`.
- Recent-errors panel, newest first, with its source described as a ring buffer.
- Search across vendor label and account label.

**Non-functional**
- No fetch on mount, no `api` import.
- File under 200 lines (`development-rules.md`).
- Every string Vietnamese and hardcoded; comments English.

## Architecture

```
App.tsx ──props──► ProvidersView
                     ├── groupByVendor(accounts, usage, pool) → VendorRow[]  → VendorCard × N
                     └── recentErrors(accounts, usage, 20)    → RecentErrorRow[] → RecentErrors
```

## Related code files

- **Create:** `web-next/src/components/ProvidersView.tsx`
- **Create:** `web-next/src/components/ProvidersView.test.tsx`
- **Modify:** none
- **Delete:** none

## Implementation steps

### Step 1: Write the failing test

Create `web-next/src/components/ProvidersView.test.tsx`:

```tsx
import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ProvidersView } from './ProvidersView'
import type { Account, PeriodSummary, PoolHealth, UsageStats } from '../lib/api'

function account(over: Partial<Account> & { id: string }): Account {
  return { enabled: true, ...over }
}

function summary(over: Partial<PeriodSummary> = {}): PeriodSummary {
  return { requests: 0, promptTokens: 0, completionTokens: 0, ...over }
}

// Two accounts on one host with the same provider label ("External OpenAI"
// collapses every external vendor), one account on a second host, one whose
// catalog refresh just failed, and one with no baseUrl at all — the five shapes
// the page has to keep apart.
const accounts: Account[] = [
  account({ id: 'go-1', nickname: 'Gorouter One', baseUrl: 'https://gorouter.app/', provider: 'External OpenAI', requestCount: 900 }),
  account({ id: 'go-2', baseUrl: 'https://gorouter.app/', provider: 'External OpenAI', requestCount: 100 }),
  account({ id: 'tab-1', nickname: 'Tab One', baseUrl: 'https://tabitoken.com/', provider: 'External OpenAI', requestCount: 40, modelCount: 12, catalogState: 'ready', catalogSource: 'builtin' }),
  // Still serving its last-known models, but the refresh that would confirm them
  // failed — the case modelCount alone cannot express.
  account({ id: 'fx-1', nickname: 'Fx One', baseUrl: 'https://fxqidian.de5.net/', modelCount: 3, catalogState: 'stale', catalogError: 'loadCodeAssist: HTTP 401: invalid metadata.platform' }),
  account({ id: 'codex-1', nickname: 'Codex Key', provider: 'OpenAI Codex', authMethod: 'codex' }),
]

const usage: UsageStats = {
  requests: 0, promptTokens: 0, completionTokens: 0, cost: 0,
  totalRequests: 0, totalPromptTokens: 0, totalCompletionTokens: 0, totalCost: 0,
  byModel: {}, byAPIKey: {}, byEndpoint: {},
  byAccount: {
    'go-1': summary({ requests: 30, errors: 6 }),
    // tab-1 served requests but has no error count: a server older than the
    // errors field omits a zero. It must render "0", never "—".
    'tab-1': summary({ requests: 10 }),
    'codex-1': summary({ requests: 4, errors: 1 }),
  },
  recentRequests: [
    { accountId: 'tab-1', status: 'error', provider: 'External OpenAI', model: 'gpt-5', error: 'upstream 500', timestamp: '2026-09-15T10:00:00Z' },
    { accountId: 'go-1', status: 'success', provider: 'External OpenAI', model: 'qwen3.8-max' },
  ],
}

const coolingPool: PoolHealth = {
  since: 1_760_000_000,
  uptimeSeconds: 600,
  accounts: {
    'go-1': { cooldownUntil: 1_760_000_900, cooldownReason: 'auth_failed', consecutiveErrors: 4, modelLocks: { 'qwen3.8-max': { until: 1_760_000_600, reason: 'rate_limited' } } },
  },
}

const props = { accounts, usage, pool: null, period: '24h', onPeriod: () => {}, onReload: () => {} }

describe('ProvidersView', () => {
  it('renders one card per vendor, grouped by host', () => {
    render(<ProvidersView {...props} />)

    expect(within(screen.getByTestId('vendor-host:gorouter.app')).getByText(/2 tài khoản/)).toBeTruthy()
    expect(within(screen.getByTestId('vendor-host:tabitoken.com')).getByText(/1 tài khoản/)).toBeTruthy()
    // No baseUrl, so it groups by provider label instead of landing in an
    // empty-string bucket.
    expect(screen.getByTestId('vendor-provider:OpenAI Codex')).toBeTruthy()
  })

  it('shows the vendor API link', () => {
    render(<ProvidersView {...props} />)
    expect(within(screen.getByTestId('vendor-host:gorouter.app')).getByText('https://gorouter.app/')).toBeTruthy()
  })

  // The falsy trap: AccountsView's KV renders String(v||'—'), which turns a
  // legitimate zero into an em dash. On this page that reads as "no data".
  // Asserted against the dd that follows each label, so a legitimate '—' in some
  // other row cannot mask the bug.
  it('renders a zero error count as "0", not as an em dash', () => {
    render(<ProvidersView {...props} />)
    const card = screen.getByTestId('vendor-host:tabitoken.com')

    expect(within(card).getByText('Lỗi (kỳ)').nextElementSibling?.textContent).toBe('0')
    expect(within(card).getByText('Lỗi (luỹ kế)').nextElementSibling?.textContent).toBe('0')
  })

  it('sums the period counts of the accounts in the group', () => {
    render(<ProvidersView {...props} />)
    const card = screen.getByTestId('vendor-host:gorouter.app')

    // go-1: 30 requests / 6 errors. go-2 served nothing, so it is absent from
    // byAccount and contributes zero rather than NaN.
    expect(within(card).getByText('30')).toBeTruthy()
    expect(within(card).getByText('6')).toBeTruthy()
    expect(within(card).getByText('20.0%')).toBeTruthy()
  })

  // The API publishes five separate catalog fields because modelCount alone
  // cannot tell "no chat catalog" from "credential just died" from "never
  // probed" (proxy/handler.go:7991-7994). The card shows the count *and* the
  // state, and repeats the error verbatim — a truncated upstream message is
  // still the most useful thing on the card.
  it('shows catalog health: the count with its state, and the error verbatim', () => {
    render(<ProvidersView {...props} />)

    expect(within(screen.getByTestId('vendor-host:tabitoken.com')).getByText('12 · ready')).toBeTruthy()

    const broken = within(screen.getByTestId('vendor-host:fxqidian.de5.net'))
    expect(broken.getByText('3 · lỗi catalog')).toBeTruthy()
    expect(broken.getByText('loadCodeAssist: HTTP 401: invalid metadata.platform')).toBeTruthy()

    // codex-1 has no catalog fields at all: no count, so no state is claimed.
    expect(within(screen.getByTestId('vendor-provider:OpenAI Codex')).getByText('Chưa nạp')).toBeTruthy()
  })

  it('calls onPeriod with the selected period', () => {
    const onPeriod = vi.fn()
    render(<ProvidersView {...props} onPeriod={onPeriod} />)

    fireEvent.click(screen.getByRole('button', { name: '7 ngày' }))
    expect(onPeriod).toHaveBeenCalledWith('7d')
  })

  it('marks a vendor whose account is in cooldown, with the reason', () => {
    render(<ProvidersView {...props} pool={coolingPool} />)
    const card = screen.getByTestId('vendor-host:gorouter.app')

    expect(within(card).getByText('Đang bị chặn')).toBeTruthy()
    expect(within(card).getByText(/Sai hoặc hết hạn xác thực/)).toBeTruthy()
    expect(within(card).getByText(/qwen3\.8-max/)).toBeTruthy()
  })

  // The record's own `provider` field says "External OpenAI" for every external
  // vendor, so attributing by it would file this failure under gorouter.app.
  it('attributes a recent failure by accountId, not by the record provider label', () => {
    render(<ProvidersView {...props} />)
    const panel = screen.getByText('Lỗi gần đây').closest('section')!

    expect(within(panel).getByText('Tab One')).toBeTruthy()
    expect(within(panel).getByText('tabitoken.com')).toBeTruthy()
    expect(within(panel).getByText('upstream 500')).toBeTruthy()
    expect(within(panel).queryByText('External OpenAI')).toBeNull()
  })

  it('filters vendors by label and by account name', () => {
    render(<ProvidersView {...props} />)
    const search = screen.getByLabelText('Tìm nhà cung cấp')

    fireEvent.change(search, { target: { value: 'gorouter one' } })
    expect(screen.getByTestId('vendor-host:gorouter.app')).toBeTruthy()
    expect(screen.queryByTestId('vendor-host:tabitoken.com')).toBeNull()
  })

  it('never renders a credential-shaped value', () => {
    // The markers are short and deliberately unshaped: the assertion is that no
    // credential-bearing *field* reaches the DOM, and a realistic-looking
    // literal here would only trip the repo's pre-commit secret scanner.
    const withSecrets = [account({ id: 'go-1', baseUrl: 'https://gorouter.app/', accessToken: 'tok-1-LEAK', apiKey: 'key-1-LEAK', extKeyMasked: 'msk-1-LEAK' })]
    render(<ProvidersView {...props} accounts={withSecrets} />)

    expect(document.body.textContent).not.toContain('LEAK')
  })

  it('shows the empty state when nothing failed', () => {
    render(<ProvidersView {...props} usage={{ ...usage, recentRequests: [] }} />)
    expect(screen.getByText('Chưa ghi nhận lỗi nào trong bộ đệm.')).toBeTruthy()
  })
})
```

### Step 2: Run to verify it fails

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- ProvidersView.test.tsx 2>&1 | tail -20
```

Expected: fails to resolve `./ProvidersView`.

### Step 3: Write the view

Create `web-next/src/components/ProvidersView.tsx`:

```tsx
import { useMemo, useState } from 'react'
import type { Account, PoolHealth, UsageStats } from '../lib/api'
import { accountLabel, exactNumber, health, relativeTime } from '../lib/format'
import { groupByVendor, recentErrors, vendorErrorRate, type RecentErrorRow, type VendorRow } from '../lib/providers'
import { Card, PageHeader } from './Shell'

/** No `1h`: getPeriodCutoff defaults an unknown period to 24h while
 *  dailyCutoffDate defaults it to 7 days, so the backend cannot serve it. */
const PERIODS = [['24h', '24 giờ'], ['7d', '7 ngày'], ['30d', '30 ngày']] as const

/** Keys are CooldownClass.String() from pool/cooldown_class.go. An unmapped one
 *  renders verbatim rather than being hidden, so a new class shows up instead
 *  of silently reading as "không rõ". */
const REASON_TEXT: Record<string, string> = {
  auth_failed: 'Sai hoặc hết hạn xác thực',
  rate_limited: 'Bị giới hạn tốc độ',
  no_balance: 'Hết số dư',
  kiro_truncated: 'Kiro cắt ngữ cảnh',
  transient: 'Lỗi tạm thời',
}
const reasonText = (reason: string) => REASON_TEXT[reason] || reason || 'Không rõ'

const statusText = { active: 'Đang phục vụ', idle: 'Chưa có lưu lượng', disabled: 'Đã tắt', banned: 'Bị khoá' } as const
const statusTone = { active: 'bg-emerald-50 text-emerald-700', idle: 'bg-sky-50 text-sky-700', disabled: 'bg-slate-100 text-slate-600', banned: 'bg-red-50 text-red-700' } as const

export function ProvidersView({ accounts, usage, pool, period, onPeriod, onReload }: {
  accounts: Account[]
  usage: UsageStats | null
  pool: PoolHealth | null
  period: string
  onPeriod: (period: string) => void
  onReload: () => Promise<void> | void
}) {
  const [query, setQuery] = useState('')
  const [busy, setBusy] = useState(false)
  const rows = useMemo(() => groupByVendor(accounts, usage, pool), [accounts, usage, pool])
  const errors = useMemo(() => recentErrors(accounts, usage, 20), [accounts, usage])
  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return rows
    return rows.filter((row) => row.label.toLowerCase().includes(needle) || row.accounts.some((a) => accountLabel(a).toLowerCase().includes(needle)))
  }, [rows, query])
  const totalRequests = rows.reduce((sum, row) => sum + row.requests, 0)
  const totalErrors = rows.reduce((sum, row) => sum + row.errors, 0)
  const blocked = rows.filter((row) => row.cooledDown > 0 || row.lockedModels.length > 0).length

  async function reload() {
    setBusy(true)
    try { await onReload() } finally { setBusy(false) }
  }

  return <div className="mx-auto max-w-7xl">
    <PageHeader
      title="Nhà cung cấp"
      description={`Mỗi upstream một thẻ: API link, số lần gọi, lỗi và tình trạng pool. ${rows.length} nhà cung cấp · ${exactNumber(accounts.length)} tài khoản.`}
      action={<button className="btn-primary" disabled={busy} onClick={() => void reload()}>{busy ? 'Đang làm mới…' : 'Làm mới'}</button>}
    />
    <div className="mb-4 flex flex-wrap items-center gap-2">
      {PERIODS.map(([value, label]) => <button key={value} onClick={() => onPeriod(value)} aria-pressed={period === value} className={`rounded-lg border px-3 py-2 text-sm ${period === value ? 'border-blue-600 bg-blue-600 font-semibold text-white' : 'border-slate-300 bg-white text-slate-600 hover:bg-slate-50'}`}>{label}</button>)}
      <input className="input min-w-56 flex-1" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Tìm nhà cung cấp hoặc tài khoản…" aria-label="Tìm nhà cung cấp" />
      <span className="text-xs text-slate-500">Kỳ này: {exactNumber(totalRequests)} lượt gọi · {exactNumber(totalErrors)} lỗi{blocked > 0 ? ` · ${blocked} đang bị chặn` : ''}</span>
    </div>
    <p className="mb-4 text-xs leading-5 text-slate-500">
      Nhãn chặn và lý do đọc từ bộ nhớ tiến trình, tính từ lúc khởi động {pool ? relativeTime(pool.since) : '—'}. Sau khi khởi động lại, mọi nhà cung cấp đều hiện bình thường cho tới khi phát sinh lỗi mới. Số lỗi theo kỳ chỉ đúng kể từ khi bộ đếm này được ghi; số luỹ kế là cộng dồn từ trước tới nay.
    </p>
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">{shown.map((row) => <div key={row.key} data-testid={`vendor-${row.key}`}><VendorCard row={row} /></div>)}</div>
    {!shown.length && <Card className="p-10 text-center text-sm text-slate-500">Không có nhà cung cấp phù hợp.</Card>}
    <RecentErrors rows={errors} />
  </div>
}

function VendorCard({ row }: { row: VendorRow }) {
  const rate = vendorErrorRate(row)
  const roster = row.accounts.reduce<Record<string, number>>((acc, a) => { const key = health(a); acc[key] = (acc[key] || 0) + 1; return acc }, {})
  const statusKeys = Object.keys(statusText) as Array<keyof typeof statusText>
  const baseUrl = row.accounts[0]?.baseUrl

  return <Card className="h-full p-5">
    <div className="flex items-start justify-between gap-2">
      <div className="min-w-0">
        <h2 className="truncate text-sm font-semibold" title={row.label}>{row.label}</h2>
        <p className="mt-0.5 text-[11px] text-slate-500">{row.kind === 'host' ? 'Upstream' : 'Theo xác thực'} · {row.accounts.length} tài khoản</p>
      </div>
      {row.cooledDown > 0 || row.lockedModels.length > 0
        ? <span className="shrink-0 rounded-full bg-red-50 px-2 py-1 text-[11px] font-medium text-red-700">Đang bị chặn</span>
        : row.atRisk > 0
          ? <span className="shrink-0 rounded-full bg-amber-50 px-2 py-1 text-[11px] font-medium text-amber-700">Sắp bị chặn</span>
          : null}
    </div>
    {baseUrl ? <p className="mt-2 truncate font-mono text-[11px] text-blue-700" title={baseUrl}>{baseUrl}</p> : null}
    <dl className="mt-3 space-y-1.5">
      <KV k="Gọi (kỳ)" v={exactNumber(row.requests)} />
      <KV k="Lỗi (kỳ)" v={exactNumber(row.errors)} />
      <KV k="Tỉ lệ lỗi" v={rate == null ? 'Chưa có dữ liệu' : `${(rate * 100).toFixed(1)}%`} />
      <KV k="Token (kỳ)" v={exactNumber(row.promptTokens + row.completionTokens)} />
      <KV k="Chi phí (kỳ)" v={row.realCost > 0 ? row.realCost.toFixed(4) : '—'} />
      <KV k="Gọi (luỹ kế)" v={exactNumber(row.lifetimeRequests)} />
      <KV k="Lỗi (luỹ kế)" v={exactNumber(row.lifetimeErrors)} />
      <KV k="Model" v={row.modelCount > 0 ? `${exactNumber(row.modelCount)} · ${catalogLabel(row)}` : 'Chưa nạp'} />
    </dl>
    {catalogError(row) ? <p className="mt-2 break-words rounded-lg bg-amber-50 px-3 py-2 text-[11px] leading-5 text-amber-800">{catalogError(row)}</p> : null}
    <div className="mt-3 flex flex-wrap gap-1">{statusKeys.filter((key) => roster[key]).map((key) => <span key={key} className={`rounded-full px-2 py-0.5 text-[11px] ${statusTone[key]}`}>{roster[key]} {statusText[key].toLowerCase()}</span>)}</div>
    {row.cooldownUntil > 0 ? <p className="mt-3 rounded-lg bg-red-50 px-3 py-2 text-[11px] leading-5 text-red-800">Mở lại {relativeTime(row.cooldownUntil)} · {row.cooldownReasons.map(reasonText).join(', ')}</p> : null}
    {row.lockedModels.length > 0 ? <ul className="mt-2 space-y-1">{row.lockedModels.slice(0, 3).map((lock) => <li key={`${lock.accountId}-${lock.model}`} className="truncate text-[11px] text-amber-800" title={`${lock.model} · ${reasonText(lock.reason)}`}>Khoá model <span className="font-mono">{lock.model}</span> tới {relativeTime(lock.until)} · {reasonText(lock.reason)}</li>)}{row.lockedModels.length > 3 ? <li className="text-[11px] text-slate-500">+{row.lockedModels.length - 3} model khác</li> : null}</ul> : null}
  </Card>
}

function RecentErrors({ rows }: { rows: RecentErrorRow[] }) {
  return <Card className="mt-5 overflow-hidden">
    <div className="border-b border-slate-200 px-5 py-4">
      <h2 className="text-sm font-semibold">Lỗi gần đây</h2>
      <p className="mt-1 text-xs text-slate-500">Nguồn là bộ đệm 500 request gần nhất trong bộ nhớ, không phải một khoảng thời gian — sau khi khởi động lại sẽ trống.</p>
    </div>
    {rows.length === 0
      ? <p className="p-5 text-sm text-slate-500">Chưa ghi nhận lỗi nào trong bộ đệm.</p>
      : <ul className="divide-y divide-slate-100">{rows.map((row, index) => <li key={`${row.accountId}-${index}`} className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-5 py-3 text-xs">
        <span className="font-medium">{row.vendorLabel}</span>
        <span className="text-slate-500">{row.accountLabel}</span>
        {row.model ? <span className="font-mono text-slate-500">{row.model}</span> : null}
        <span className="ml-auto text-slate-400">{relativeTime(Date.parse(row.timestamp) / 1000)}</span>
        <p className="w-full break-words text-red-700">{row.error || 'Không có thông điệp'}</p>
      </li>)}</ul>}
  </Card>
}

/** `v == null || v === ''` rather than AccountsView's `String(v||'—')`: a count
 *  of zero is a result, not a missing value, and this page exists to report it. */
function KV({ k, v }: { k: string; v: unknown }) {
  return <div className="grid grid-cols-[110px_1fr] gap-3 text-xs"><dt className="text-slate-500">{k}</dt><dd className="min-w-0 break-words text-right font-medium tabular-nums">{v == null || v === '' ? '—' : String(v)}</dd></div>
}

/** Catalog state collapses across the group: a vendor is only as healthy as its
 *  accounts, and the server publishes exactly these fields because "a service
 *  provider with no chat catalog, a provider whose credential just died, and one
 *  that was never probed all report zero" (proxy/handler.go:7991-7994). */
function catalogLabel(row: VendorRow): string {
  if (row.accounts.some((a) => a.catalogError)) return 'lỗi catalog'
  const state = row.accounts.find((a) => a.catalogState)?.catalogState
  const source = row.accounts.find((a) => a.catalogSource)?.catalogSource
  return state || source || 'chưa rõ'
}
function catalogError(row: VendorRow): string {
  return row.accounts.find((a) => a.catalogError)?.catalogError || ''
}
```

### Step 4: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- ProvidersView.test.tsx 2>&1 | tail -25
```

Expected: 10 passed.

If `renders one card per vendor` fails on the account count, check
`groupByVendor`'s grouping before touching the view — the bug is in phase 03,
not here.

### Step 5: Run the whole suite

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test 2>&1 | tail -25
```

Expected: all suites pass. `AccountsView`, `ApiView`, `LogsView` and `QuotaView`
are unaffected — this phase modifies no existing file.

### Step 6: Typecheck and lint

```bash
cd /Users/van/Tools/OmniProxy/web-next
npx tsc -b 2>&1 | tail -20
npm run lint 2>&1 | tail -15
```

Expected: no output from `tsc -b`, no errors from oxlint.

### Step 7: Confirm the line-count rule

```bash
cd /Users/van/Tools/OmniProxy/web-next
wc -l src/components/ProvidersView.tsx
```

Expected: under 200. If over, move `VendorCard` and `RecentErrors` into
`ProvidersView.parts.tsx` and import them — do not shorten by deleting the
health disclosure.

### Step 8: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/components/ProvidersView.tsx web-next/src/components/ProvidersView.test.tsx
git commit -F - <<'EOF'
feat(web-next): render a card per vendor with calls, errors and pool health

The admin UI could list every account but not every vendor, so an operator
diagnosing a bad upstream saw 70+ flat rows and had to infer which one was at
fault. Each vendor now gets a card: API link, period and lifetime call and
error counts, the model count, the account roster by health, and the live
cooldown with its reason.

The local KV tests for null and an empty string instead of copying
AccountsView's String(v||'—'). That version renders a legitimate zero as an em
dash, which on this page is the difference between "no errors" and "no data".

The period selector offers only 24h, 7d and 30d. The backend cannot serve 1h:
getPeriodCutoff defaults an unknown period to 24h while dailyCutoffDate
defaults it to 7 days, so that selection would show three different windows
under one label.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `ProvidersView.test.tsx` with all 10 tests
- [ ] Step 2: confirm it fails to resolve `./ProvidersView`
- [ ] Step 3: write `ProvidersView.tsx`
- [ ] Step 4: all 10 tests pass
- [ ] Step 5: full suite still green
- [ ] Step 6: `tsc -b` and `npm run lint` clean
- [ ] Step 7: file under 200 lines
- [ ] Step 8: commit

## Success criteria

- 10 tests pass and the four pre-existing suites are untouched.
- A vendor with zero errors renders `"0"`, not `"—"`.
- A vendor whose catalog refresh failed shows the failure, and one that was never
  probed shows `Chưa nạp` rather than a bare `0`.
- The recent-errors panel attributes a record by `accountId`, so two vendors
  whose records both say `provider: 'External OpenAI'` appear as two rows.
- No credential-shaped value reaches the DOM.
- The period selector never offers `1h`.

## Risk assessment

| Risk | Mitigation |
|---|---|
| Zero rendered as an em dash | Dedicated test pins the `dd` after `Lỗi (kỳ)` / `Lỗi (luỹ kế)` to `'0'`, so a legitimate `—` in another row cannot mask it |
| `health` prop shadowing `format.ts`'s `health()` | Prop is named `pool` |
| Recent error attributed by the record's coarse `provider` label | Test asserts the panel shows the vendor host and never `'External OpenAI'` |
| A dead catalog read as a healthy one, because `modelCount` is the only field rendered | `catalogLabel` reports `lỗi catalog` whenever any account carries a `catalogError`, and `catalogError` repeats the message verbatim; both are exercised by the catalog test |
| 20+ vendor cards unreadable at `xl:grid-cols-4` | Search box plus the attention-first sort from phase 03 |
| File exceeds 200 lines | `wc -l` gate; extraction path named in step 7 |
| Using `.chip`/`.btn-danger-soft`, which no stylesheet defines | Period selector built from inline Tailwind; the only class-based button is `.btn-primary`, which exists |

## Security considerations

- **Read-only.** The view has no mutating action: no account edit, no credential
  field, no `api.*` call. `onReload` refreshes the parent's data.
- **No credential reaches the DOM.** A test renders an account carrying
  `accessToken`, `apiKey` and `extKeyMasked` and asserts none of those values
  appear in `document.body.textContent`.
- **Upstream error strings are rendered as text.** React escapes them. They can
  contain an account email, already the case in the Usage view today.
- **The health panel cannot be read as durable state.** The disclosure line names
  the process start time and states that a restart resets it — a panel that goes
  green on restart would otherwise be read as evidence.

## Next steps

Phase 05 registers the section: `'providers'` in the `Section` union and in
`items`, `VALID_SECTIONS`, the `load()` branch that calls `api.usage` and
`api.poolHealth`, and one ternary arm passing `accounts`, `usage`, `pool`,
`period`, `onPeriod` and `onReload`.
