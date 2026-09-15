import type { PeriodSummary, UsageStats } from './api'

export type UsageDimension = 'model' | 'account' | 'apiKey' | 'endpoint'
export type UsageMetric = 'tokens' | 'cost'

export interface ShareSlice {
  name: string
  value: number
}

/** Compute share slices for a given dimension and metric. Filters out zero
 *  values before summing so the centre total equals the drawn slices. Slices
 *  are sorted descending by value. */
export function shareByDimension(
  usage: UsageStats | null,
  dimension: UsageDimension,
  metric: UsageMetric,
): { slices: ShareSlice[]; total: number } {
  if (!usage) return { slices: [], total: 0 }
  const map: Record<string, PeriodSummary> =
    dimension === 'model'
      ? usage.byModel
      : dimension === 'account'
        ? usage.byAccount
        : dimension === 'apiKey'
          ? usage.byAPIKey
          : usage.byEndpoint

  const raw: ShareSlice[] = []
  for (const [name, s] of Object.entries(map)) {
    const value =
      metric === 'tokens'
        ? (s.promptTokens ?? 0) + (s.completionTokens ?? 0)
        : (s.realCost ?? s.cost ?? 0)
    if (value > 0) raw.push({ name, value })
  }
  raw.sort((a, b) => b.value - a.value)
  const total = raw.reduce((sum, s) => sum + s.value, 0)
  return { slices: raw, total }
}
