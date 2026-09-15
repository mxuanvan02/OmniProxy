import { Card } from './Shell'
import type { KpiCard } from '../lib/usage'

const TONE_CLASS: Record<string, string> = {
  ok: 'text-emerald-700',
  warn: 'text-amber-700',
  bad: 'text-red-700',
}

export function UsageKpiRow({ cards }: { cards: KpiCard[] }) {
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      {cards.map((c) => (
        <div key={c.key} data-testid={`kpi-${c.key}`}>
          <Card className="p-5">
            <div className="text-xs font-medium uppercase tracking-wide text-slate-500">{c.label}</div>
            <div className={`mt-2 font-mono text-2xl font-semibold tabular-nums ${c.tone ? TONE_CLASS[c.tone] ?? 'text-slate-950' : 'text-slate-950'}`}>
              {c.value}
            </div>
            <div className="mt-1 text-[11px] text-slate-500">{c.note}</div>
          </Card>
        </div>
      ))}
    </div>
  )
}
