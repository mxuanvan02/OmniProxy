import { useState } from 'react'
import { Card } from './Shell'
import { compactNumber, exactNumber } from '../lib/format'
import { periodErrors, shareByDimension, type UsageDimension, type UsageMetric } from '../lib/usage'
import type { ChartPoint, UsageStats } from '../lib/api'
import { Bar, BarChart, CartesianGrid, Cell, Legend, Pie, PieChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

const DIMENSIONS: [UsageDimension, string][] = [['model', 'Model'], ['account', 'Tài khoản'], ['apiKey', 'API key'], ['endpoint', 'Endpoint']]]
const METRICS: [UsageMetric, string][] = [['tokens', 'Token'], ['cost', 'Chi phí']]
const COLORS = ['#2563eb', '#7c3aed', '#0891b2', '#059669', '#d97706', '#dc2626', '#db2777', '#4f46e5']
const MAX_SLICES = 8

export function UsageChartPanel({ usage, chart }: { usage: UsageStats | null; chart: ChartPoint[] }) {
  const [dimension, setDimension] = useState<UsageDimension>('model')
  const [metric, setMetric] = useState<UsageMetric>('tokens')

  const errors = periodErrors(usage)
  const { slices, total } = shareByDimension(usage, dimension, metric)
  const displaySlices = slices.length > MAX_SLICES
    ? [...slices.slice(0, MAX_SLICES), { name: 'Khác', value: slices.slice(MAX_SLICES).reduce((s, x) => s + x.value, 0) }]
    : slices

  return (
    <div data-testid="usage-chart-panel">
      <Card className="p-5">
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <div>
            <span className="mr-2 text-xs font-medium uppercase tracking-wide text-slate-500">Cơ cấu theo</span>
            <div className="inline-flex rounded-lg border border-slate-200 bg-slate-50 p-0.5">
              {DIMENSIONS.map(([k, label]) => (
                <button key={k} onClick={() => setDimension(k)} aria-pressed={dimension === k}
                  className={`rounded-md px-3 py-1 text-xs font-medium transition-colors ${dimension === k ? 'bg-white text-slate-950 shadow-sm' : 'text-slate-500 hover:text-slate-700'}`}>
                  {label}
                </button>
              ))}
            </div>
          </div>
          <div className="inline-flex rounded-lg border border-slate-200 bg-slate-50 p-0.5">
            {METRICS.map(([k, label]) => (
              <button key={k} onClick={() => setMetric(k)} aria-pressed={metric === k}
                className={`rounded-md px-3 py-1 text-xs font-medium transition-colors ${metric === k ? 'bg-white text-slate-950 shadow-sm' : 'text-slate-500 hover:text-slate-700'}`}>
                {label}
              </button>
            ))}
          </div>
        </div>

        <div className="grid gap-6 xl:grid-cols-[1.4fr_.6fr]">
          <div>
            <h2 className="mb-1 font-semibold">Xu hướng theo thời gian</h2>
            <p className="mb-4 text-xs text-slate-500">Biểu đồ chỉ có một chuỗi: máy chủ không trả số request hoặc lỗi theo từng bucket.</p>
            {chart.length > 0 ? (
              <div className="h-64" data-testid="chart-area">
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={chart}>
                    <CartesianGrid strokeDasharray="3 3" vertical={false} />
                    <XAxis dataKey="label" tick={{ fontSize: 11 }} minTickGap={28} />
                    <YAxis tickFormatter={compactNumber} tick={{ fontSize: 11 }} width={48} />
                    <Tooltip formatter={(v) => metric === 'tokens' ? [exactNumber(Number(v)), 'Token'] : [`$${Number(v).toFixed(4)}`, 'Chi phí']} />
                    <Bar dataKey={metric === 'tokens' ? 'tokens' : 'cost'} fill="#2563eb" radius={[2, 2, 0, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </div>
            ) : (
              <div className="flex h-64 items-center justify-center text-sm text-slate-500">Chưa có dữ liệu trong kỳ.</div>
            )}
          </div>

          <div>
            <h2 className="mb-1 font-semibold">Phân bổ</h2>
            <p className="mb-4 text-xs text-slate-500">Lỗi trong kỳ: <span className="tabular-nums">{errors}</span></p>
            {displaySlices.length > 0 ? (
              <div className="relative h-64">
                <ResponsiveContainer width="100%" height="100%">
                  <PieChart>
                    <Pie innerRadius={50} outerRadius={80} data={displaySlices} dataKey="value" paddingAngle={1}>
                      {displaySlices.map((_, i) => <Cell key={i} fill={COLORS[i % COLORS.length]} />)}
                    </Pie>
                    <Legend formatter={(v: string) => <span className="text-xs text-slate-600">{v}</span>} />
                    <Tooltip formatter={(v) => metric === 'tokens' ? [exactNumber(Number(v)), 'Token'] : [`$${Number(v).toFixed(4)}`, 'Chi phí']} />
                  </PieChart>
                </ResponsiveContainer>
                <div data-testid="donut-total" className="absolute inset-0 flex items-center justify-center pointer-events-none">
                  <div className="text-center">
                    <div className="font-mono text-lg font-bold tabular-nums text-slate-950">
                      {metric === 'tokens' ? compactNumber(total) : `$${total.toFixed(2)}`}
                    </div>
                    <div className="text-[10px] text-slate-500">Tổng</div>
                  </div>
                </div>
              </div>
            ) : (
              <div className="flex h-64 items-center justify-center text-sm text-slate-500">Kỳ này chưa có lưu lượng để tính cơ cấu.</div>
            )}
          </div>
        </div>
      </Card>
    </div>
  )
}
