import { useMemo, useState } from 'react'
import type { Account, PoolHealth, UsageStats } from '../lib/api'
import { accountLabel, exactNumber, health, relativeTime } from '../lib/format'
import { groupByVendor, recentErrors, vendorErrorRate, type RecentErrorRow, type VendorRow } from '../lib/providers'
import { USAGE_PERIODS } from '../lib/usage'
import { Card, PageHeader } from './Shell'

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
      {USAGE_PERIODS.map(([value, label]) => <button key={value} onClick={() => onPeriod(value)} aria-pressed={period === value} className={`rounded-lg border px-3 py-2 text-sm ${period === value ? 'border-blue-600 bg-blue-600 font-semibold text-white' : 'border-slate-300 bg-white text-slate-600 hover:bg-slate-50'}`}>{label}</button>)}
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
