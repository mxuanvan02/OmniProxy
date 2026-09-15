import { Card } from './Shell'
import { compactNumber, exactNumber, relativeTime } from '../lib/format'
import { modelRows } from '../lib/usage'
import type { UsageStats } from '../lib/api'

export function UsageModelsTable({ usage }: { usage: UsageStats | null }) {
  const rows = modelRows(usage, 12)
  return (
    <Card className="overflow-hidden">
      <div className="border-b border-slate-200 p-5"><h2 className="font-semibold">Model theo lưu lượng</h2></div>
      {rows.length > 0 ? (
        <table className="data-table" data-testid="models-table">
          <thead><tr><th>Model</th><th className="num">Token</th><th>Tỷ lệ</th><th className="num">Lỗi</th><th>Lần gọi gần nhất</th></tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.model}>
                <td className="max-w-48 truncate font-mono text-xs" title={r.model}>{r.model}</td>
                <td className="num" title={exactNumber(r.tokens)}>{compactNumber(r.tokens)}</td>
                <td><div className="flex items-center gap-2"><div className="h-1.5 w-24 overflow-hidden rounded-full bg-slate-100"><div className="h-full rounded-full bg-blue-600" style={{ width: `${Math.min(r.share * 100, 100)}%` }} /></div><span className="text-xs tabular-nums text-slate-500">{(r.share * 100).toFixed(1)}%</span></div></td>
                <td className="num">{String(r.errors)}</td>
                <td className="text-xs text-slate-500">{relativeTime(r.lastCallAt ?? undefined)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <div className="p-8 text-center text-sm text-slate-500">Chưa có dữ liệu model trong kỳ.</div>
      )}
    </Card>
  )
}
