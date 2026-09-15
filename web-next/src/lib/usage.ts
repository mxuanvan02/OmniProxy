import type { RecentRequest, Status, UsageStats } from './api'
export { shareByDimension, type ShareSlice, type UsageDimension, type UsageMetric } from './usage-dimensions'
export { recentWindow, requestRows, type WindowRow, type RequestRow } from './usage-rows'

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/** The periods the backend serves for this UI. No `1h`: dailyData is keyed by
 *  UTC day so no sub-day total can be aggregated, and the chart has no bucket
 *  finer than one hour. Must match `usagePeriods` in proxy/usage_tracker.go. */
export const USAGE_PERIODS = [
  ['24h', '24 giờ'],
  ['7d', '7 ngày'],
  ['30d', '30 ngày'],
] as const

export const USAGE_DIMENSIONS = [
  ['model', 'Model'],
  ['account', 'Tài khoản'],
  ['apiKey', 'API key'],
  ['endpoint', 'Endpoint'],
] as const

/** Capacity of the tracker's recent-requests ring buffer. Exported so panels
 *  can name the limit in their disclosure. See usage_tracker.go:194. */
export const RING_CAPACITY = 500

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface HeroStats {
  successTokens: number
  totalRequests: number
  failedRequests: number
  lastCallAt: number | null
  activeRequests: number
  activeCredentials: number
  totalCredentials: number
  modelCount: number
}

export interface KpiCard {
  key: string
  label: string
  value: string
  note: string
  tone?: 'ok' | 'warn' | 'bad'
}

export interface ModelRow {
  model: string
  requests: number
  tokens: number
  share: number
  errors: number
  lastCallAt: number | null
}

// ---------------------------------------------------------------------------
// Pure derivations
// ---------------------------------------------------------------------------

/** Parse a record timestamp to unix seconds. Returns null when absent or
 *  unparseable — never NaN, so callers can compare safely. */
export function parseRecordTime(ts?: string): number | null {
  if (!ts) return null
  const ms = Date.parse(ts)
  return Number.isFinite(ms) ? ms / 1000 : null
}

/** Sum of byModel errors. UsageStats does not embed PeriodSummary on the wire,
 *  so there is no root-level errors field — summing byModel is the only way. */
export function periodErrors(usage: UsageStats | null): number {
  if (!usage) return 0
  let sum = 0
  for (const s of Object.values(usage.byModel)) {
    sum += s.errors ?? 0
  }
  return sum
}

export function heroStats(usage: UsageStats | null, status: Status | null): HeroStats {
  const u = usage
  const s = status
  return {
    successTokens: u?.totalCompletionTokens ?? 0,
    totalRequests: u?.totalRequests ?? 0,
    failedRequests: periodErrors(u),
    lastCallAt: (() => {
      if (!u?.recentRequests) return null
      for (const r of u.recentRequests) {
        const t = parseRecordTime(r.timestamp)
        if (t !== null) return t
      }
      return null
    })(),
    activeRequests: u?.activeRequests?.length ?? 0,
    activeCredentials: s?.available ?? 0,
    totalCredentials: s?.totalAccounts ?? 0,
    modelCount: s?.availableModels ?? 0,
  }
}

export function kpiCards(usage: UsageStats | null, status: Status | null): KpiCard[] {
  const u = usage
  const s = status
  const errs = periodErrors(u)
  return [
    {
      key: 'credentials',
      label: 'Tài khoản sẵn sàng',
      value: `${s?.available ?? 0}/${s?.totalAccounts ?? 0}`,
      note: 'Đang hoạt động / tổng số',
      tone: (s?.available ?? 0) === 0 ? 'bad' : undefined,
    },
    {
      key: 'cache',
      label: 'Token cache đọc',
      value: formatCompact(u?.totalCacheReadTokens ?? 0),
      note: 'Tiết kiệm chi phí nhờ prompt caching',
    },
    {
      key: 'errors',
      label: 'Request lỗi',
      value: String(errs),
      note: 'Tổng lỗi trong kỳ từ byModel',
      tone: errs > 0 ? 'bad' : undefined,
    },
    {
      key: 'effective',
      label: 'Token hiệu dụng',
      value: formatCompact(u?.totalEffectiveTokens ?? 0),
      note: 'Đã trừ cache: (input − cached) + output',
    },
  ]
}

export function modelRows(usage: UsageStats | null, limit: number): ModelRow[] {
  if (!usage) return []
  const total = usage.totalRequests
  const entries = Object.entries(usage.byModel).sort((a, b) => b[1].requests - a[1].requests)
  return entries.slice(0, limit).map(([model, s]) => ({
    model,
    requests: s.requests,
    tokens: (s.promptTokens ?? 0) + (s.completionTokens ?? 0),
    share: total > 0 ? s.requests / total : 0,
    errors: s.errors ?? 0,
    lastCallAt: newestTimestampForModel(usage.recentRequests, model),
  }))
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function newestTimestampForModel(records: RecentRequest[] | undefined, model: string): number | null {
  if (!records) return null
  let best: number | null = null
  for (const r of records) {
    if (r.model !== model) continue
    const t = parseRecordTime(r.timestamp)
    if (t !== null && (best === null || t > best)) best = t
  }
  return best
}

function formatCompact(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`
  return String(n)
}
