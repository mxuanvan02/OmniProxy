import type { RecentRequest, UsageStats } from './api'
import { parseRecordTime, RING_CAPACITY } from './usage'

export interface WindowRow {
  accountId: string
  label: string
  calls: number
  errors: number
  tokens: number
  lastCallAt: number | null
}

export interface RequestRow {
  timestamp: string | undefined
  key: string
  model: string
  credential: string
  status: string
  error: string
  tokenTotal: number
  cost: number | null
}

/** Filter recentRequests to the last N minutes, group by accountId. Returns
 *  truncated=true when the ring is at capacity and may have dropped older
 *  in-window records. */
export function recentWindow(
  usage: UsageStats | null,
  minutes: number,
  now: number,
): { rows: WindowRow[]; truncated: boolean } {
  if (!usage?.recentRequests) return { rows: [], truncated: false }
  const cutoff = now - minutes * 60
  const inWindow: RecentRequest[] = []
  for (const r of usage.recentRequests) {
    const t = parseRecordTime(r.timestamp)
    if (t !== null && t >= cutoff) inWindow.push(r)
  }
  const truncated =
    inWindow.length >= RING_CAPACITY && usage.recentRequests.length >= RING_CAPACITY

  const groups = new Map<string, { records: RecentRequest[]; label: string }>()
  for (const r of inWindow) {
    const id = r.accountId ?? ''
    if (!groups.has(id)) {
      groups.set(id, {
        records: [],
        label: r.accountName || id.slice(0, 8) || '—',
      })
    }
    groups.get(id)!.records.push(r)
  }

  const rows: WindowRow[] = []
  for (const [accountId, g] of groups) {
    let tokens = 0
    let errors = 0
    let lastCallAt: number | null = null
    for (const r of g.records) {
      tokens += (r.inputTokens ?? 0) + (r.outputTokens ?? 0)
      if (r.status === 'error') errors++
      const t = parseRecordTime(r.timestamp)
      if (t !== null && (lastCallAt === null || t > lastCallAt)) lastCallAt = t
    }
    rows.push({ accountId, label: g.label, calls: g.records.length, errors, tokens, lastCallAt })
  }
  rows.sort((a, b) => (b.lastCallAt ?? 0) - (a.lastCallAt ?? 0))
  return { rows, truncated }
}

/** Return the most recent requests sorted newest-first, capped at limit. */
export function requestRows(usage: UsageStats | null, limit: number): RequestRow[] {
  if (!usage?.recentRequests) return []
  // Sort explicitly so the contract holds even if upstream ordering changes.
  const sorted = [...usage.recentRequests].sort((a, b) => {
    const ta = parseRecordTime(a.timestamp) ?? 0
    const tb = parseRecordTime(b.timestamp) ?? 0
    return tb - ta
  })
  return sorted.slice(0, limit).map((r) => ({
    timestamp: r.timestamp,
    key: r.apiKeyId || '—',
    model: r.model ?? '—',
    credential: r.accountName || (r.accountId ?? '').slice(0, 8) || '—',
    status: r.status ?? '—',
    error: r.error ?? '',
    tokenTotal: (r.inputTokens ?? 0) + (r.outputTokens ?? 0),
    cost: r.realCost ?? null,
  }))
}
