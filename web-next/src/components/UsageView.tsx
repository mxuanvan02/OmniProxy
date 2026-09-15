import { PageHeader } from './Shell'
import { UsageHero } from './UsageHero'
import { UsageKpiRow } from './UsageKpiRow'
import { UsageChartPanel } from './UsageChartPanel'
import { UsageModelsTable } from './UsageModelsTable'
import { UsageProvidersPanel } from './UsageProvidersPanel'
import { UsageRequestsTable } from './UsageRequestsTable'
import { heroStats, kpiCards, USAGE_PERIODS } from '../lib/usage'
import type { ChartPoint, Status, UsageStats } from '../lib/api'

export function UsageView({ usage, chart, period, onPeriod, status, updatedAt }: {
  usage: UsageStats | null; chart: ChartPoint[]; period: string
  onPeriod: (p: string) => void; status: Status | null; updatedAt: number | null
}) {
  const hero = heroStats(usage, status)
  const cards = kpiCards(usage, status)
  // updatedAt is wall-clock milliseconds (App.tsx sets Date.now()); recentWindow
  // compares against record timestamps in unix seconds, so convert once here.
  // It is null only before the first load completes — exactly when usage is also
  // null — so 0 is a safe pure fallback and keeps Date.now() out of render.
  const now = (updatedAt ?? 0) / 1000

  return (
    <div className="mx-auto max-w-7xl space-y-5">
      <PageHeader
        title="Sử dụng"
        description="Lưu lượng, token và chi phí theo cùng một khoảng thời gian."
        action={
          <div className="inline-flex rounded-lg border border-slate-200 bg-slate-50 p-0.5">
            {USAGE_PERIODS.map(([value, label]) => (
              <button key={value} onClick={() => onPeriod(value)} aria-pressed={period === value}
                className={`rounded-md px-3 py-1 text-xs font-medium transition-colors ${period === value ? 'bg-white text-slate-950 shadow-sm' : 'text-slate-500 hover:text-slate-700'}`}>
                {label}
              </button>
            ))}
          </div>
        }
      />
      <UsageHero stats={hero} totalRealCost={usage?.totalRealCost ?? 0} totalCost={usage?.totalCost ?? 0} />
      <UsageKpiRow cards={cards} />
      <UsageChartPanel usage={usage} chart={chart} />
      <UsageModelsTable usage={usage} />
      <UsageProvidersPanel usage={usage} now={now} />
      <UsageRequestsTable usage={usage} />
      <p className="text-right"><a className="text-xs font-medium text-blue-700 hover:underline" href="/admin/#usage">Request details, cache và compression trong UI cũ ↗</a></p>
    </div>
  )
}
