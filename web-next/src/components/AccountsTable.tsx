import { useMemo, useRef, useState } from 'react'
import {
  createColumnHelper,
  flexRender,
  getCoreRowModel,
  getFilteredRowModel,
  getSortedRowModel,
  useReactTable,
  type SortingState,
} from '@tanstack/react-table'
import { useVirtualizer } from '@tanstack/react-virtual'
import type { Account, AccountCapability, CapabilitySummary } from '../lib/api'
import { accountLabel, compactNumber, errorRate, exactNumber, health, providerLabel, relativeTime, type Health } from '../lib/format'

/** Status is carried by a shape (the dot) plus text, not by colour alone —
 *  colour-only encoding fails WCAG 1.4.1 and red/green is the most common
 *  confusion pair. `disabled` is deliberately grey: 33 of 49 accounts are off,
 *  so colouring that state paints the column with noise and buries the 15 rows
 *  that are actually live. */
const HEALTH_DOT: Record<Health, string> = {
  active: 'bg-emerald-600 dark:bg-emerald-400',
  idle: 'bg-sky-600 dark:bg-sky-400',
  disabled: 'bg-neutral-400 dark:bg-neutral-500',
  banned: 'bg-red-600 dark:bg-red-400',
}

const HEALTH_TEXT: Record<Health, string> = {
  active: 'text-emerald-800 dark:text-emerald-300',
  idle: 'text-sky-800 dark:text-sky-300',
  disabled: 'text-neutral-500 dark:text-neutral-400',
  banned: 'text-red-800 dark:text-red-300',
}

const HEALTH_LABEL: Record<Health, string> = {
  active: 'Đang chạy',
  idle: 'Chưa dùng',
  disabled: 'Đã tắt',
  banned: 'Bị khoá',
}

/** Numeric columns are right-aligned so digits line up by place value; that is
 *  what lets the eye compare magnitudes down a column instead of reading every
 *  cell. Kept as an id set rather than TanStack column meta to avoid a module
 *  augmentation for one boolean. */
const NUMERIC_COLUMNS = new Set(['models', 'requests', 'errors', 'tokens', 'credits'])

const NUM = 'font-mono text-sm tabular-nums'

const column = createColumnHelper<Account>()
const CHEAP_PROBE_CAPABILITIES = new Set(['embedding', 'moderation'])

type AccountsTableProps = {
  accounts: Account[]
  capabilityAccounts: AccountCapability[]
  capabilityCatalog: CapabilitySummary[]
  onProbe: (accountId: string) => Promise<void>
  probingAccountId: string
}

export function AccountsTable({
  accounts,
  capabilityAccounts,
  capabilityCatalog,
  onProbe,
  probingAccountId,
}: AccountsTableProps) {
  const [globalFilter, setGlobalFilter] = useState('')
  const [healthFilter, setHealthFilter] = useState<'all' | Health>('all')
  const [sorting, setSorting] = useState<SortingState>([{ id: 'requests', desc: true }])

  const capabilityByAccount = useMemo(
    () => new Map(capabilityAccounts.map((item) => [item.id, item])),
    [capabilityAccounts],
  )
  const endpointsByCapability = useMemo(
    () => new Map(capabilityCatalog.map((item) => [item.capability, item.endpoints ?? []])),
    [capabilityCatalog],
  )

  const columns = useMemo(
    () => [
      column.accessor((a) => accountLabel(a), {
        id: 'name',
        header: 'Tài khoản',
        size: 280,
        cell: (ctx) => (
          <div className="min-w-0">
            <div className="truncate font-medium text-neutral-900 dark:text-neutral-100">{ctx.getValue()}</div>
            <div
              title={ctx.row.original.baseUrl || undefined}
              className="truncate font-mono text-xs text-neutral-600 dark:text-neutral-400"
            >
              {ctx.row.original.baseUrl || ctx.row.original.authMethod || ctx.row.original.id.slice(0, 8)}
            </div>
          </div>
        ),
      }),
      column.accessor((a) => providerLabel(a), {
        id: 'provider',
        header: 'Nhà cung cấp',
        size: 190,
        cell: (ctx) => (
          <span className="block truncate text-neutral-700 dark:text-neutral-300" title={ctx.getValue()}>
            {ctx.getValue()}
          </span>
        ),
      }),
      column.accessor((a) => capabilityByAccount.get(a.id)?.effective?.join(' ') ?? '', {
        id: 'capabilities',
        header: 'Capability / endpoint',
        size: 380,
        cell: (ctx) => {
          const account = ctx.row.original
          const detail = capabilityByAccount.get(account.id)
          const capabilities = detail?.effective ?? []
          const canProbe =
            account.enabled &&
            health(account) !== 'banned' &&
            capabilities.some((capability) => CHEAP_PROBE_CAPABILITIES.has(capability))
          const isProbing = probingAccountId === account.id

          if (capabilities.length === 0) {
            return <span className="text-xs text-neutral-400">Không quảng bá capability</span>
          }

          return (
            <div className="flex min-w-72 items-center gap-2">
              <div className="flex min-w-0 flex-1 flex-wrap gap-1">
                {capabilities.map((capability) => {
                  const probe = detail?.probes?.[capability]
                  const endpoints = endpointsByCapability.get(capability) ?? []
                  const state = probe?.ok ? 'verified' : probe?.skipped ? 'skipped' : probe ? 'failed' : 'advertised'
                  const tone =
                    state === 'verified'
                      ? 'border-emerald-300 bg-emerald-50 text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/30 dark:text-emerald-300'
                      : state === 'failed'
                        ? 'border-red-300 bg-red-50 text-red-800 dark:border-red-900 dark:bg-red-950/30 dark:text-red-300'
                        : 'border-neutral-300 bg-neutral-50 text-neutral-700 dark:border-neutral-700 dark:bg-neutral-950 dark:text-neutral-300'
                  const title = [
                    `${capability}: ${state}`,
                    ...endpoints,
                    probe?.detail || probe?.skippedReason || '',
                  ]
                    .filter(Boolean)
                    .join('\n')

                  return (
                    <span key={capability} title={title} className={`rounded border px-1.5 py-0.5 font-mono text-[11px] ${tone}`}>
                      {probe?.ok ? '✓ ' : probe && !probe.skipped ? '✗ ' : ''}
                      {capability}
                    </span>
                  )
                })}
              </div>
              <button
                type="button"
                disabled={!canProbe || Boolean(probingAccountId)}
                title={
                  canProbe
                    ? 'Probe miễn phí embedding/moderation'
                    : 'Chỉ bật cho account hoạt động có embedding hoặc moderation'
                }
                onClick={() => void onProbe(account.id)}
                className="shrink-0 rounded border border-neutral-300 px-2 py-1 text-xs hover:bg-neutral-100 disabled:cursor-not-allowed disabled:opacity-40 dark:border-neutral-700 dark:hover:bg-neutral-800"
              >
                {isProbing ? 'Đang probe…' : 'Probe'}
              </button>
            </div>
          )
        },
      }),
      column.accessor((a) => health(a), {
        id: 'health',
        header: 'Trạng thái',
        size: 130,
        cell: (ctx) => {
          const value = ctx.getValue()
          return (
            <span className={`inline-flex items-center gap-1.5 text-xs font-medium ${HEALTH_TEXT[value]}`}>
              <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${HEALTH_DOT[value]}`} />
              {HEALTH_LABEL[value]}
            </span>
          )
        },
      }),
      column.accessor((a) => a.modelCount ?? 0, {
        id: 'models',
        header: 'Model',
        size: 90,
        cell: (ctx) => {
          const a = ctx.row.original
          const failed = Boolean(a.catalogError)
          return (
            <span
              title={a.catalogError || a.catalogSource || ''}
              className={`${NUM} ${failed ? 'text-red-700 dark:text-red-400' : 'text-neutral-700 dark:text-neutral-300'}`}
            >
              {ctx.getValue()}
              {failed ? ' !' : ''}
            </span>
          )
        },
      }),
      column.accessor((a) => (a.requestCount ?? 0) + (a.serviceRequestCount ?? 0), {
        id: 'requests',
        header: 'Request',
        size: 100,
        cell: (ctx) => (
          <span className={NUM} title={exactNumber(ctx.getValue())}>
            {compactNumber(ctx.getValue())}
          </span>
        ),
      }),
      column.accessor((a) => errorRate(a) ?? -1, {
        id: 'errors',
        header: 'Tỉ lệ lỗi',
        size: 100,
        cell: (ctx) => {
          const rate = errorRate(ctx.row.original)
          if (rate === null) return <span className={`${NUM} text-neutral-400`} title="Chưa có request">—</span>
          // Any non-zero rate is an anomaly worth seeing: in this pool almost
          // every row is exactly 0.0%, so a flat grey column hides the one
          // account that is actually failing.
          const tone =
            rate >= 0.2
              ? 'text-red-700 dark:text-red-400 font-medium'
              : rate > 0
                ? 'text-amber-700 dark:text-amber-400'
                : 'text-neutral-500 dark:text-neutral-500'
          return <span className={`${NUM} ${tone}`}>{(rate * 100).toFixed(1)}%</span>
        },
      }),
      column.accessor((a) => a.totalTokens ?? 0, {
        id: 'tokens',
        header: 'Token',
        size: 100,
        cell: (ctx) => (
          <span className={`${NUM} text-neutral-700 dark:text-neutral-300`} title={exactNumber(ctx.getValue())}>
            {compactNumber(ctx.getValue())}
          </span>
        ),
      }),
      column.accessor((a) => a.extCreditsRemaining ?? 0, {
        id: 'credits',
        header: 'Credit còn',
        size: 110,
        cell: (ctx) => {
          const a = ctx.row.original
          // '—' and '0.00' mean different things: never checked vs. checked and
          // empty. The title carries that distinction, which colour alone cannot.
          if (!a.extCreditsCheckedAt) {
            return (
              <span className={`${NUM} text-neutral-400`} title="Chưa kiểm tra credit">
                —
              </span>
            )
          }
          return (
            <span
              className={`${NUM} text-neutral-700 dark:text-neutral-300`}
              title={`Kiểm tra ${relativeTime(a.extCreditsCheckedAt)}`}
            >
              {(a.extCreditsRemaining ?? 0).toFixed(2)}
            </span>
          )
        },
      }),
      column.accessor((a) => Math.max(a.lastUsed ?? 0, a.serviceLastUsed ?? 0), {
        id: 'lastUsed',
        header: 'Lần cuối',
        size: 130,
        cell: (ctx) => (
          <span className="text-sm text-neutral-600 dark:text-neutral-400">{relativeTime(ctx.getValue() || undefined)}</span>
        ),
      }),
    ],
    [capabilityByAccount, endpointsByCapability, onProbe, probingAccountId],
  )

  const rows = useMemo(
    () => (healthFilter === 'all' ? accounts : accounts.filter((a) => health(a) === healthFilter)),
    [accounts, healthFilter],
  )

  const table = useReactTable({
    data: rows,
    columns,
    state: { sorting, globalFilter },
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
    globalFilterFn: 'includesString',
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
  })

  const scrollRef = useRef<HTMLDivElement>(null)
  const tableRows = table.getRowModel().rows
  const virtualizer = useVirtualizer({
    count: tableRows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 56,
    overscan: 12,
  })

  const virtualRows = virtualizer.getVirtualItems()
  const padTop = virtualRows.length > 0 ? virtualRows[0].start : 0
  const padBottom =
    virtualRows.length > 0 ? virtualizer.getTotalSize() - virtualRows[virtualRows.length - 1].end : 0

  const counts = useMemo(() => {
    const acc: Record<Health, number> = { active: 0, idle: 0, disabled: 0, banned: 0 }
    for (const a of accounts) acc[health(a)] += 1
    return acc
  }, [accounts])

  return (
    <section className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-wrap items-center gap-3 pb-3">
        <input
          value={globalFilter}
          onChange={(e) => setGlobalFilter(e.target.value)}
          placeholder="Tìm theo tên, email, nhà cung cấp…"
          aria-label="Tìm tài khoản"
          className="w-72 rounded-lg border border-neutral-300 bg-white px-3 py-1.5 text-sm outline-none focus-visible:border-neutral-900 dark:border-neutral-700 dark:bg-neutral-950 dark:focus-visible:border-neutral-100"
        />

        <div className="flex items-center gap-1">
          {(['all', 'active', 'idle', 'disabled', 'banned'] as const).map((key) => {
            const on = healthFilter === key
            const label = key === 'all' ? `Tất cả (${accounts.length})` : `${HEALTH_LABEL[key]} (${counts[key]})`
            return (
              <button
                key={key}
                type="button"
                aria-pressed={on}
                onClick={() => setHealthFilter(key)}
                className={`rounded-full border px-3 py-1 text-xs font-medium ${
                  on
                    ? 'border-neutral-900 bg-neutral-900 text-white dark:border-neutral-100 dark:bg-neutral-100 dark:text-neutral-900'
                    : 'border-neutral-300 bg-white text-neutral-700 hover:bg-neutral-100 dark:border-neutral-700 dark:bg-neutral-900 dark:text-neutral-300'
                }`}
              >
                {label}
              </button>
            )
          })}
        </div>

        <span className="ml-auto font-mono text-xs tabular-nums text-neutral-600 dark:text-neutral-400">
          {tableRows.length} / {accounts.length} dòng
        </span>
      </div>

      <div
        ref={scrollRef}
        className="min-h-0 flex-1 overflow-auto rounded-xl border border-neutral-200 bg-white dark:border-neutral-800 dark:bg-neutral-900"
      >
        <table className="w-full border-collapse text-sm">
          <thead className="sticky top-0 z-10 bg-neutral-100/95 backdrop-blur dark:bg-neutral-900/95">
            {table.getHeaderGroups().map((group) => (
              <tr key={group.id}>
                {group.headers.map((header) => {
                  const sorted = header.column.getIsSorted()
                  const numeric = NUMERIC_COLUMNS.has(header.column.id)
                  return (
                    <th
                      key={header.id}
                      scope="col"
                      aria-sort={sorted === 'asc' ? 'ascending' : sorted === 'desc' ? 'descending' : 'none'}
                      style={{ width: header.getSize() }}
                      className={`border-b border-neutral-300 px-3 py-2 dark:border-neutral-700 ${numeric ? 'text-right' : 'text-left'}`}
                    >
                      {/* The sort caret sits on the leading side of a numeric
                          header so the label's right edge stays flush with the
                          digits below it. With the caret trailing, gap+width
                          pushed every numeric header 12px left of its column. */}
                      <button
                        type="button"
                        onClick={header.column.getToggleSortingHandler()}
                        className={`flex w-full items-center gap-1 text-xs font-semibold tracking-wide ${
                          numeric ? 'flex-row-reverse justify-start' : 'justify-start'
                        } ${sorted ? 'text-neutral-900 dark:text-neutral-100' : 'text-neutral-600 dark:text-neutral-400'}`}
                      >
                        {flexRender(header.column.columnDef.header, header.getContext())}
                        {/* Width is reserved unconditionally: without it the
                            label shifts sideways the moment a column is sorted. */}
                        <span aria-hidden="true" className="w-2 shrink-0">
                          {sorted === 'asc' ? '↑' : sorted === 'desc' ? '↓' : ''}
                        </span>
                      </button>
                    </th>
                  )
                })}
              </tr>
            ))}
          </thead>
          <tbody>
            {padTop > 0 ? (
              <tr>
                <td colSpan={columns.length} style={{ height: padTop }} />
              </tr>
            ) : null}

            {virtualRows.map((virtualRow) => {
              const row = tableRows[virtualRow.index]
              return (
                <tr
                  key={row.id}
                  className="border-b border-neutral-100 last:border-0 hover:bg-neutral-50 dark:border-neutral-800 dark:hover:bg-neutral-800/50"
                >
                  {row.getVisibleCells().map((cell) => (
                    <td
                      key={cell.id}
                      className={`px-3 py-2 align-middle ${NUMERIC_COLUMNS.has(cell.column.id) ? 'text-right' : 'text-left'}`}
                    >
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  ))}
                </tr>
              )
            })}

            {padBottom > 0 ? (
              <tr>
                <td colSpan={columns.length} style={{ height: padBottom }} />
              </tr>
            ) : null}

            {tableRows.length === 0 ? (
              <tr>
                <td colSpan={columns.length} className="px-3 py-10 text-center text-sm text-neutral-500">
                  Không có tài khoản khớp bộ lọc.
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>
    </section>
  )
}
