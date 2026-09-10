import { useCallback, useEffect, useMemo, useState } from 'react'
import { AccountsTable } from './components/AccountsTable'
import { Login } from './components/Login'
import { ApiError, api, getToken, logout } from './lib/api'
import type { Account } from './lib/api'
import { health } from './lib/format'

const POLL_MS = 10_000

export function App() {
  const [authed, setAuthed] = useState(() => Boolean(getToken()))
  const [accounts, setAccounts] = useState<Account[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [fetchedAt, setFetchedAt] = useState<number | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const rows = await api.accounts()
      setAccounts(Array.isArray(rows) ? rows : [])
      setFetchedAt(Date.now())
      setError('')
    } catch (err) {
      // A 401 means the session died; drop to the login shell instead of
      // showing a stale pool as if it were live.
      if (err instanceof ApiError && err.status === 401) {
        setAuthed(false)
        setAccounts([])
      }
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (!authed) return
    void load()
    const timer = window.setInterval(() => {
      // Skip polling while the tab is hidden: a background dashboard that keeps
      // re-fetching a ~76 KiB payload is the classic long-run leak.
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
    // h-dvh + flex column is what makes the table's sticky header work: the
    // scroll has to happen inside the table container, not on the window. With
    // a page-level scroll the header scrolls away no matter what CSS it carries.
    <div className="flex h-dvh flex-col overflow-hidden bg-neutral-50 text-neutral-900">
      <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-neutral-200 bg-white px-6 py-4">
        <div>
          <h1 className="text-lg font-semibold tracking-tight">OmniProxy · Accounts</h1>
          <p className="font-mono text-xs text-neutral-500">
            {accounts.length} tài khoản
            {fetchedAt ? ` · cập nhật ${new Date(fetchedAt).toLocaleTimeString('vi-VN')}` : ''}
          </p>
        </div>

        {/* Labels match the table's filter chips word for word. Two names for
            one state ("chờ" here, "Chưa dùng" there) reads as two metrics. */}
        <dl className="flex gap-4 font-mono text-xs">
          <Stat label="đang chạy" value={summary.active} tone="text-emerald-700" />
          <Stat label="chưa dùng" value={summary.idle} tone="text-sky-700" />
          <Stat label="đã tắt" value={summary.disabled} tone="text-neutral-500" />
          {/* Alarm colour only when there is something to alarm about: a red
              zero draws the eye every render and teaches the operator to ignore
              red. */}
          <Stat
            label="bị khoá"
            value={summary.banned}
            tone={summary.banned > 0 ? 'text-red-700' : 'text-neutral-500'}
          />
        </dl>

        <div className="ml-auto flex items-center gap-2">
          <button
            type="button"
            onClick={() => void load()}
            disabled={loading}
            className="rounded-md border border-neutral-300 px-3 py-1.5 text-sm hover:bg-neutral-100 disabled:opacity-50"
          >
            {loading ? 'Đang tải…' : 'Tải lại'}
          </button>
          <button
            type="button"
            onClick={async () => {
              await logout()
              setAuthed(false)
              setAccounts([])
            }}
            className="rounded-md px-3 py-1.5 text-sm text-neutral-600 hover:bg-neutral-100"
          >
            Đăng xuất
          </button>
        </div>
      </header>

      {error ? (
        <p role="alert" className="border-b border-red-200 bg-red-50 px-6 py-2 text-sm text-red-800">
          {error}
        </p>
      ) : null}

      <main className="flex min-h-0 flex-1 flex-col p-6">
        <AccountsTable accounts={accounts} />
      </main>
    </div>
  )
}

function Stat({ label, value, tone }: { label: string; value: number; tone: string }) {
  return (
    <div className="flex items-baseline gap-1.5">
      <dd className={`text-base font-semibold ${tone}`}>{value}</dd>
      <dt className="text-neutral-500">{label}</dt>
    </div>
  )
}
