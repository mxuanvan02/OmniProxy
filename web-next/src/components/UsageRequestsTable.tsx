import { Card } from './Shell'
import { compactNumber, relativeTime } from '../lib/format'
import { parseRecordTime, requestRows } from '../lib/usage'
import type { UsageStats } from '../lib/api'

export function UsageRequestsTable({ usage }: { usage: UsageStats | null }) {
  const rows = requestRows(usage, 20)
  return (
    <Card className="overflow-hidden">
      <div className="border-b border-slate-200 p-5">
        <h2 className="font-semibold">Request gần nhất</h2>
        <p className="mt-1 text-[11px] text-slate-500">Mỗi dòng là một request.</p>
      </div>
      {rows.length > 0 ? (
        <table className="data-table" data-testid="requests-table">
          <thead><tr><th>Thời gian</th><th>API key</th><th>Model</th><th>Tài khoản</th><th>Kết quả</th><th className="num">Token</th><th className="num">Chi phí</th></tr></thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={`${r.credential}-${i}`}>
                <td className="text-xs text-slate-500">{relativeTime(parseRecordTime(r.timestamp) ?? undefined)}</td>
                <td className="max-w-24 truncate font-mono text-xs" title={r.key}>{r.key}</td>
                <td className="max-w-32 truncate font-mono text-xs" title={r.model}>{r.model}</td>
                <td className="text-xs">{r.credential}</td>
                <td>
                  <span className={`text-xs ${r.status === 'error' ? 'text-red-700' : 'text-emerald-700'}`}>{r.status}</span>
                  {r.error && <div className="mt-0.5 max-w-xs break-words text-[10px] text-red-600">{r.error}</div>}
                </td>
                <td className="num">{compactNumber(r.tokenTotal)}</td>
                <td className="num">{r.cost !== null ? `$${r.cost.toFixed(4)}` : '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <div className="p-8 text-center text-sm text-slate-500">Chưa có request nào.</div>
      )}
    </Card>
  )
}
