import { Card } from './Shell'
import { exactNumber, relativeTime } from '../lib/format'
import { recentWindow, RING_CAPACITY } from '../lib/usage'
import type { UsageStats } from '../lib/api'

export function UsageProvidersPanel({ usage, now }: { usage: UsageStats | null; now: number }) {
  const { rows, truncated } = recentWindow(usage, 5, now)
  return (
    <Card className="overflow-hidden">
      <div className="border-b border-slate-200 p-5">
        <h2 className="font-semibold">Tài khoản — 5 phút gần nhất</h2>
        <p className="mt-1 text-[11px] text-slate-500">
          Nguồn là bộ đệm {RING_CAPACITY} request gần nhất trong bộ nhớ, không phải một cửa sổ thời gian đầy đủ — sau khi khởi động lại sẽ trống.
        </p>
      </div>
      {rows.length > 0 ? (
        <>
          <table className="data-table" data-testid="providers-table">
            <thead><tr><th>Tài khoản</th><th className="num">Gọi</th><th className="num">Lỗi</th><th className="num">Token</th><th>Lần gọi gần nhất</th></tr></thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.accountId}>
                  <td className="text-xs">{r.label}</td>
                  <td className="num">{exactNumber(r.calls)}</td>
                  <td className="num">{String(r.errors)}</td>
                  <td className="num">{exactNumber(r.tokens)}</td>
                  <td className="text-xs text-slate-500">{relativeTime(r.lastCallAt ?? undefined)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {truncated && <div className="border-t border-amber-100 bg-amber-50 px-5 py-2 text-xs text-amber-800">Bộ đệm đã đầy: cửa sổ 5 phút có thể bị cắt.</div>}
        </>
      ) : (
        <div className="p-8 text-center text-sm text-slate-500">Chưa có request nào trong 5 phút gần nhất.</div>
      )}
    </Card>
  )
}
