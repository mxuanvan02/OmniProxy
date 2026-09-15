import { Card } from './Shell'
import { compactNumber, exactNumber, relativeTime } from '../lib/format'
import type { HeroStats } from '../lib/usage'

export interface UsageHeroProps {
  stats: HeroStats
  totalRealCost: number
  totalCost: number
}

export function UsageHero({ stats, totalRealCost, totalCost }: UsageHeroProps) {
  const serving = stats.activeRequests > 0
  return (
    <div data-testid="usage-hero" className="grid gap-4 xl:grid-cols-[1fr_auto]">
      <Card className="p-6">
        <div className="text-xs font-medium uppercase tracking-wide text-slate-500">Token đầu ra</div>
        <div className="mt-2 break-all font-mono text-4xl font-bold tabular-nums text-slate-950" title={exactNumber(stats.successTokens)}>
          {compactNumber(stats.successTokens)}
        </div>
        <div className="mt-4 flex flex-wrap gap-x-6 gap-y-1 text-xs text-slate-600">
          <span>Request: <b className="tabular-nums">{exactNumber(stats.totalRequests)}</b></span>
          <span>Lỗi: <b className="tabular-nums">{exactNumber(stats.failedRequests)}</b></span>
          <span>Gần nhất: <b>{relativeTime(stats.lastCallAt ?? undefined)}</b></span>
          <span>Tài khoản: <b className="tabular-nums">{stats.activeCredentials}/{stats.totalCredentials}</b></span>
          <span>Model: <b className="tabular-nums">{stats.modelCount}</b></span>
        </div>
      </Card>
      <Card className="min-w-56 p-6">
        <div className="text-xs font-medium uppercase tracking-wide text-slate-500">Chi phí thực (USD)</div>
        <div className="mt-2 font-mono text-2xl font-semibold tabular-nums text-slate-950">
          ${totalRealCost.toFixed(3)}
        </div>
        <div className="mt-1 text-xs text-slate-500">Credit upstream: <span className="tabular-nums">${totalCost.toFixed(3)}</span></div>
        <div className="mt-4 flex items-center gap-2 text-xs">
          {serving ? (
            <><span className="inline-block h-2 w-2 animate-pulse rounded-full bg-emerald-500" /><span className="text-emerald-700">Đang phục vụ</span></>
          ) : (
            <span className="text-slate-500">Chưa có request đang chạy</span>
          )}
        </div>
        <div className="mt-1 text-[10px] text-slate-400">Cập nhật mỗi 15 giây</div>
      </Card>
    </div>
  )
}
