import { useCallback, useEffect, useMemo, useState } from 'react'
import { AccountsTable } from './components/AccountsTable'
import { CapabilityCatalog } from './components/CapabilityCatalog'
import { Login } from './components/Login'
import { ApiError, api, getToken, logout } from './lib/api'
import type { Account, CapabilityMatrix, CapabilityProbeResponse } from './lib/api'
import { health } from './lib/format'

const POLL_MS = 10_000
const EMPTY_MATRIX: CapabilityMatrix = { capabilities: [], accounts: [] }

export function App() {
  const [authed, setAuthed] = useState(() => Boolean(getToken()))
  const [accounts, setAccounts] = useState<Account[]>([])
  const [capabilityMatrix, setCapabilityMatrix] = useState<CapabilityMatrix>(EMPTY_MATRIX)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [loading, setLoading] = useState(false)
  const [probingAccountId, setProbingAccountId] = useState('')
  const [fetchedAt, setFetchedAt] = useState<number | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [rows, matrix] = await Promise.all([api.accounts(), api.capabilities()])
      setAccounts(Array.isArray(rows) ? rows : [])
      setCapabilityMatrix(matrix)
      setFetchedAt(Date.now())
      setError('')
    } catch (err) {
      // A 401 means the session died; drop to the login shell instead of
      // showing a stale pool as if it were live.
      if (err instanceof ApiError && err.status === 401) {
        setAuthed(false)
        setAccounts([])
        setCapabilityMatrix(EMPTY_MATRIX)
      }
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [])

  const probeCapabilities = useCallback(
    async (accountId: string) => {
      setProbingAccountId(accountId)
      setError('')
      setNotice('')
      try {
        const result = await api.probeCapabilities(accountId)
        setNotice(formatProbeResult(result))
        await load()
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err))
      } finally {
        setProbingAccountId('')
      }
    },
    [load],
  )

  useEffect(() => {
    if (!authed) return
    void load()
    const timer = window.setInterval(() => {
      // Skip polling while the tab is hidden: a background dashboard that keeps
      // re-fetching large payloads wastes bandwidth and can retain stale work.
      if (!document.hidden) void load()
    }, POLL_MS)
    return () => window.clearInterval(timer)
  }, [authed, load])

  const summary = useMemo(() => {
    const counts = { active: 0, idle: 0, disabled: 0, banned: 0 }
    for (const a of accounts) counts[health(a)] += 1
    return counts
  }, [accounts])

  if (!authed) {
    return <Login onDone={() => setAuthed(true)} />
  }

  return (
    <div className="flex h-dvh flex-col overflow-hidden bg-neutral-50 text-neutral-900 dark:bg-neutral-950 dark:text-neutral-100">
      <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-neutral-200 bg-white px-6 py-4 dark:border-neutral-800 dark:bg-neutral-900">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">OmniProxy · Accounts</h1>
          <p className="font-mono text-xs text-neutral-500">
            {accounts.length} tài khoản
            {fetchedAt ? ` · cập nhật ${new Date(fetchedAt).toLocaleTimeString('vi-VN')}` : ''}
          </p>
        </div>

        <dl className="flex gap-4 font-mono text-xs">
          <Stat label="đang chạy" value={summary.active} tone="text-emerald-700 dark:text-emerald-400" />
          <Stat label="chưa dùng" value={summary.idle} tone="text-sky-700 dark:text-sky-400" />
          <Stat label="đã tắt" value={summary.disabled} tone="text-neutral-500" />
          <Stat
            label="bị khoá"
            value={summary.banned}
            tone={summary.banned > 0 ? 'text-red-700 dark:text-red-400' : 'text-neutral-500'}
          />
        </dl>

        <div className="ml-auto flex items-center gap-2">
          <button
            type="button"
            onClick={() => void load()}
            disabled={loading}
            className="rounded-md border border-neutral-300 px-3 py-1.5 text-sm hover:bg-neutral-100 disabled:opacity-50 dark:border-neutral-700 dark:hover:bg-neutral-800"
          >
            {loading ? 'Đang tải…' : 'Tải lại'}
          </button>
          <button
            type="button"
            onClick={async () => {
              await logout()
              setAuthed(false)
              setAccounts([])
              setCapabilityMatrix(EMPTY_MATRIX)
            }}
            className="rounded-md px-3 py-1.5 text-sm text-neutral-600 hover:bg-neutral-100 dark:text-neutral-400 dark:hover:bg-neutral-800"
          >
            Đăng xuất
          </button>
        </div>
      </header>

      {error ? (
        <p role="alert" className="border-b border-red-200 bg-red-50 px-6 py-2 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/30 dark:text-red-300">
          {error}
        </p>
      ) : null}

      {notice ? (
        <p role="status" className="border-b border-emerald-200 bg-emerald-50 px-6 py-2 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/30 dark:text-emerald-300">
          {notice}
        </p>
      ) : null}

      <main className="flex min-h-0 flex-1 flex-col p-6">
        <CapabilityCatalog capabilities={capabilityMatrix.capabilities} />
        <AccountsTable
          accounts={accounts}
          capabilityAccounts={capabilityMatrix.accounts}
          capabilityCatalog={capabilityMatrix.capabilities}
          onProbe={probeCapabilities}
          probingAccountId={probingAccountId}
        />
      </main>
    </div>
  )
}

function formatProbeResult(result: CapabilityProbeResponse): string {
  const parts: string[] = []
  if (result.verified.length > 0) parts.push(`xác minh: ${result.verified.join(', ')}`)
  if (result.failed.length > 0) parts.push(`lỗi: ${result.failed.join(', ')}`)
  if (result.skipped.length > 0) parts.push(`bỏ qua: ${result.skipped.join(', ')}`)
  return parts.length > 0
    ? `Probe hoàn tất · ${parts.join(' · ')}`
    : result.note || 'Không có capability miễn phí phù hợp để probe.'
}

function Stat({ label, value, tone }: { label: string; value: number; tone: string }) {
  return (
    <div className="flex items-baseline gap-1.5">
      <dd className={`text-base font-semibold ${tone}`}>{value}</dd>
      <dt className="text-neutral-500">{label}</dt>
    </div>
  )
}
