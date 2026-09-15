import { describe, expect, it } from 'vitest'
import type { PeriodSummary, RecentRequest, Status, UsageStats } from './api'
import {
  USAGE_PERIODS,
  heroStats,
  kpiCards,
  modelRows,
  parseRecordTime,
  periodErrors,
  recentWindow,
  requestRows,
  shareByDimension,
} from './usage'

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

function summary(overrides: Partial<PeriodSummary> = {}): PeriodSummary {
  return { requests: 10, promptTokens: 100, completionTokens: 50, ...overrides }
}

function usage(overrides: Partial<UsageStats> = {}): UsageStats {
  return {
    requests: 100,
    promptTokens: 1000,
    completionTokens: 500,
    totalRequests: 100,
    totalPromptTokens: 1000,
    totalCompletionTokens: 500,
    totalCost: 0.5,
    byModel: {},
    byAccount: {},
    byAPIKey: {},
    byEndpoint: {},
    ...overrides,
  }
}

function status(overrides: Partial<Status> = {}): Status {
  return {
    accounts: 3,
    available: 2,
    totalAccounts: 5,
    totalRequests: 100,
    successRequests: 95,
    failedRequests: 5,
    totalTokens: 1500,
    totalCredits: 0.5,
    availableModels: 12,
    modelIds: [],
    uptime: 3600,
    ...overrides,
  }
}

function record(overrides: Partial<RecentRequest> = {}): RecentRequest {
  return {
    timestamp: '2026-09-15T10:00:00Z',
    model: 'gemini-3-flash',
    provider: 'External OpenAI',
    accountId: 'acct-aaaaaaaa',
    accountName: 'Alice',
    status: 'ok',
    inputTokens: 100,
    outputTokens: 50,
    realCost: 0.001,
    ...overrides,
  }
}

// ---------------------------------------------------------------------------
// USAGE_PERIODS
// ---------------------------------------------------------------------------

describe('USAGE_PERIODS', () => {
  it('never contains 1h', () => {
    const values = USAGE_PERIODS.map(([v]) => v) as string[]
    expect(values.includes('1h')).toBe(false)
  })

  it('contains exactly the values phase 01 serves for this UI', () => {
    const values = USAGE_PERIODS.map(([v]) => v).sort()
    expect(values).toEqual(['24h', '30d', '7d'])
  })
})

// ---------------------------------------------------------------------------
// periodErrors
// ---------------------------------------------------------------------------

describe('periodErrors', () => {
  it('sums byModel errors across models', () => {
    const u = usage({
      byModel: { a: summary({ errors: 3 }), b: summary({ errors: 7 }) },
    })
    expect(periodErrors(u)).toBe(10)
  })

  it('returns 0 for null usage', () => {
    expect(periodErrors(null)).toBe(0)
  })

  it('returns 0 when no summary carries an errors key', () => {
    const u = usage({ byModel: { a: summary(), b: summary() } })
    expect(periodErrors(u)).toBe(0)
  })

  it('does not read a root-level errors field', () => {
    // UsageStats does not embed PeriodSummary on the wire, so `errors` at the
    // root is undefined even if the TS interface says otherwise. A fixture with
    // root errors: 99 and no byModel errors must still return 0.
    const u = usage({ errors: 99 } as unknown as UsageStats)
    expect(periodErrors(u)).toBe(0)
  })
})

// ---------------------------------------------------------------------------
// heroStats
// ---------------------------------------------------------------------------

describe('heroStats', () => {
  it('successTokens is usage.totalCompletionTokens', () => {
    const h = heroStats(usage({ totalCompletionTokens: 42 }), status())
    expect(h.successTokens).toBe(42)
  })

  it('lastCallAt is the first parseable recentRequests timestamp', () => {
    const u = usage({
      recentRequests: [record({ timestamp: '2026-09-15T10:00:00Z' })],
    })
    const h = heroStats(u, status())
    expect(h.lastCallAt).toBe(new Date('2026-09-15T10:00:00Z').getTime() / 1000)
  })

  it('lastCallAt scans past unparseable timestamps', () => {
    const u = usage({
      recentRequests: [
        record({ timestamp: 'not a date' }),
        record({ timestamp: '2026-09-15T09:00:00Z' }),
      ],
    })
    const h = heroStats(u, status())
    expect(h.lastCallAt).toBe(new Date('2026-09-15T09:00:00Z').getTime() / 1000)
  })

  it('activeCredentials/totalCredentials/modelCount come from Status', () => {
    const h = heroStats(usage(), status({ available: 4, totalAccounts: 8, availableModels: 20 }))
    expect(h.activeCredentials).toBe(4)
    expect(h.totalCredentials).toBe(8)
    expect(h.modelCount).toBe(20)
  })

  it('every field is 0/null for null usage and null status', () => {
    const h = heroStats(null, null)
    expect(h.successTokens).toBe(0)
    expect(h.totalRequests).toBe(0)
    expect(h.failedRequests).toBe(0)
    expect(h.lastCallAt).toBeNull()
    expect(h.activeRequests).toBe(0)
    expect(h.activeCredentials).toBe(0)
    expect(h.totalCredentials).toBe(0)
    expect(h.modelCount).toBe(0)
  })
})

// ---------------------------------------------------------------------------
// kpiCards
// ---------------------------------------------------------------------------

describe('kpiCards', () => {
  it('returns exactly four cards', () => {
    expect(kpiCards(usage(), status())).toHaveLength(4)
  })

  it('the fourth card is effective tokens, not a billing claim', () => {
    const cards = kpiCards(usage({ totalEffectiveTokens: 1234 }), status())
    const fourth = cards[3]
    expect(fourth.label).toContain('hiệu dụng')
    expect(fourth.label.toLowerCase()).not.toContain('hoá đơn')
    expect(fourth.note.length).toBeGreaterThan(0)
  })

  it('contains no card whose label mentions unbilled', () => {
    const cards = kpiCards(usage(), status())
    for (const c of cards) {
      expect(c.label.toLowerCase()).not.toContain('unbilled')
      expect(c.label.toLowerCase()).not.toContain('chưa xuất')
    }
  })
})

// ---------------------------------------------------------------------------
// modelRows
// ---------------------------------------------------------------------------

describe('modelRows', () => {
  it('sorted by requests descending', () => {
    const u = usage({
      byModel: {
        a: summary({ requests: 5 }),
        b: summary({ requests: 20 }),
        c: summary({ requests: 10 }),
      },
    })
    const rows = modelRows(u, 10)
    expect(rows.map((r) => r.model)).toEqual(['b', 'c', 'a'])
  })

  it('share is requests/totalRequests, 0 when totalRequests is 0', () => {
    const u = usage({
      totalRequests: 0,
      byModel: { a: summary({ requests: 5 }) },
    })
    const rows = modelRows(u, 10)
    expect(rows[0].share).toBe(0)
    expect(Number.isFinite(rows[0].share)).toBe(true)
  })

  it('errors is 0 for a summary without the field', () => {
    const u = usage({ byModel: { a: summary() } })
    expect(modelRows(u, 10)[0].errors).toBe(0)
  })

  it('lastCallAt is newest matching recentRequests timestamp', () => {
    const u = usage({
      byModel: { a: summary() },
      recentRequests: [
        record({ model: 'a', timestamp: '2026-09-15T08:00:00Z' }),
        record({ model: 'a', timestamp: '2026-09-15T10:00:00Z' }),
        record({ model: 'b', timestamp: '2026-09-15T11:00:00Z' }),
      ],
    })
    const rows = modelRows(u, 10)
    expect(rows[0].lastCallAt).toBe(new Date('2026-09-15T10:00:00Z').getTime() / 1000)
  })

  it('lastCallAt is null when ring holds no record for that model', () => {
    const u = usage({
      byModel: { a: summary() },
      recentRequests: [record({ model: 'b' })],
    })
    expect(modelRows(u, 10)[0].lastCallAt).toBeNull()
  })

  it('respects limit', () => {
    const u = usage({
      byModel: { a: summary(), b: summary(), c: summary() },
    })
    expect(modelRows(u, 2)).toHaveLength(2)
  })
})

// ---------------------------------------------------------------------------
// recentWindow
// ---------------------------------------------------------------------------

describe('recentWindow', () => {
  const NOW = new Date('2026-09-15T10:05:00Z').getTime() / 1000

  it('excludes a record older than the window', () => {
    const u = usage({
      recentRequests: [
        record({ timestamp: '2026-09-15T09:58:00Z', accountId: 'a' }), // 7 min ago
        record({ timestamp: '2026-09-15T10:03:00Z', accountId: 'b' }), // 2 min ago
      ],
    })
    const w = recentWindow(u, 5, NOW)
    expect(w.rows).toHaveLength(1)
    expect(w.rows[0].accountId).toBe('b')
  })

  it('excludes a record whose timestamp is absent or unparseable', () => {
    const u = usage({
      recentRequests: [
        record({ timestamp: undefined, accountId: 'a' }),
        record({ timestamp: 'not a date', accountId: 'b' }),
        record({ timestamp: '2026-09-15T10:03:00Z', accountId: 'c' }),
      ],
    })
    const w = recentWindow(u, 5, NOW)
    expect(w.rows).toHaveLength(1)
    expect(w.rows[0].accountId).toBe('c')
  })

  it('groups by accountId, never by provider', () => {
    const u = usage({
      recentRequests: [
        record({ accountId: 'a', accountName: 'Alice', provider: 'External OpenAI', timestamp: '2026-09-15T10:03:00Z' }),
        record({ accountId: 'b', accountName: 'Bob', provider: 'External OpenAI', timestamp: '2026-09-15T10:04:00Z' }),
      ],
    })
    const w = recentWindow(u, 5, NOW)
    expect(w.rows).toHaveLength(2)
    const labels = w.rows.map((r) => r.label)
    expect(labels).toContain('Alice')
    expect(labels).toContain('Bob')
    expect(labels.every((l) => l !== 'External OpenAI')).toBe(true)
  })

  it('truncated is true when ring holds RING_CAPACITY in-window records', () => {
    // Build exactly 500 records inside the window
    const recs: RecentRequest[] = []
    for (let i = 0; i < 500; i++) {
      recs.push(record({ accountId: `a-${i}`, timestamp: '2026-09-15T10:03:00Z' }))
    }
    const u = usage({ recentRequests: recs })
    const w = recentWindow(u, 5, NOW)
    expect(w.truncated).toBe(true)
  })

  it('truncated is false when under capacity', () => {
    const u = usage({
      recentRequests: [record({ timestamp: '2026-09-15T10:03:00Z' })],
    })
    expect(recentWindow(u, 5, NOW).truncated).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// requestRows
// ---------------------------------------------------------------------------

describe('requestRows', () => {
  it('newest first', () => {
    const u = usage({
      recentRequests: [
        record({ timestamp: '2026-09-15T09:00:00Z' }),
        record({ timestamp: '2026-09-15T10:00:00Z' }),
      ],
    })
    const rows = requestRows(u, 10)
    expect(rows[0].timestamp).toBe('2026-09-15T10:00:00Z')
  })

  it('tokenTotal is inputTokens + outputTokens', () => {
    const u = usage({
      recentRequests: [record({ inputTokens: 100, outputTokens: 50 })],
    })
    expect(requestRows(u, 10)[0].tokenTotal).toBe(150)
  })

  it('key is — when apiKeyId is absent', () => {
    const u = usage({ recentRequests: [record()] })
    expect(requestRows(u, 10)[0].key).toBe('—')
  })

  it('key is the apiKeyId when present', () => {
    const u = usage({ recentRequests: [record({ apiKeyId: 'key-abc' })] })
    expect(requestRows(u, 10)[0].key).toBe('key-abc')
  })

  it('credential falls back to accountId.slice(0,8)', () => {
    const u = usage({
      recentRequests: [record({ accountName: undefined, accountId: 'abcdefghijk' })],
    })
    expect(requestRows(u, 10)[0].credential).toBe('abcdefgh')
  })

  it('respects limit', () => {
    const u = usage({
      recentRequests: [record(), record(), record()],
    })
    expect(requestRows(u, 2)).toHaveLength(2)
  })
})

// ---------------------------------------------------------------------------
// shareByDimension
// ---------------------------------------------------------------------------

describe('shareByDimension', () => {
  it('slices sorted descending, every value > 0', () => {
    const u = usage({
      byModel: {
        a: summary({ promptTokens: 10, completionTokens: 5 }),
        b: summary({ promptTokens: 30, completionTokens: 20 }),
      },
    })
    const { slices } = shareByDimension(u, 'model', 'tokens')
    expect(slices[0].value).toBeGreaterThan(slices[1].value)
    expect(slices.every((s) => s.value > 0)).toBe(true)
  })

  it('metric cost reads realCost, falling back to cost', () => {
    const u = usage({
      byModel: {
        a: { requests: 1, promptTokens: 0, completionTokens: 0, realCost: 0.5, cost: 1 },
        b: { requests: 1, promptTokens: 0, completionTokens: 0, cost: 0.3 },
      },
    })
    const { slices } = shareByDimension(u, 'model', 'cost')
    const aSlice = slices.find((s) => s.name === 'a')!
    expect(aSlice.value).toBe(0.5)
  })

  it('total is sum of slices, not of whole map', () => {
    const u = usage({
      byModel: {
        a: summary({ promptTokens: 10, completionTokens: 0 }),
        b: summary({ promptTokens: 0, completionTokens: 0 }), // zero → filtered
      },
    })
    const { slices, total } = shareByDimension(u, 'model', 'tokens')
    expect(total).toBe(slices.reduce((sum, s) => sum + s.value, 0))
  })

  it('returns empty slices and total 0 for null usage', () => {
    const { slices, total } = shareByDimension(null, 'model', 'tokens')
    expect(slices).toEqual([])
    expect(total).toBe(0)
  })
})

// ---------------------------------------------------------------------------
// parseRecordTime
// ---------------------------------------------------------------------------

describe('parseRecordTime', () => {
  it('returns unix seconds for an RFC3339 string', () => {
    expect(parseRecordTime('2026-09-15T10:00:00Z')).toBe(
      new Date('2026-09-15T10:00:00Z').getTime() / 1000,
    )
  })

  it('returns null for undefined, empty string, and invalid', () => {
    expect(parseRecordTime(undefined)).toBeNull()
    expect(parseRecordTime('')).toBeNull()
    expect(parseRecordTime('not a date')).toBeNull()
  })
})
