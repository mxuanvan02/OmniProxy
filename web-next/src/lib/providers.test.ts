import { describe, expect, it } from 'vitest'
import type { Account, PeriodSummary, PoolHealth, UsageStats } from './api'
import { groupByVendor, hostOf, recentErrors, vendorErrorRate, vendorKey, vendorLabel } from './providers'

function account(over: Partial<Account> & { id: string }): Account {
  return { enabled: true, ...over }
}

function summary(over: Partial<PeriodSummary> = {}): PeriodSummary {
  return { requests: 0, promptTokens: 0, completionTokens: 0, cost: 0, ...over }
}

function usage(over: Partial<UsageStats> = {}): UsageStats {
  return {
    requests: 0, promptTokens: 0, completionTokens: 0, cost: 0,
    totalRequests: 0, totalPromptTokens: 0, totalCompletionTokens: 0, totalCost: 0,
    byModel: {}, byAccount: {}, byAPIKey: {}, byEndpoint: {},
    ...over,
  }
}

describe('hostOf', () => {
  it('strips the scheme, the path, the userinfo and the trailing slash', () => {
    expect(hostOf('https://gorouter.app/')).toBe('gorouter.app')
    expect(hostOf('http://gorouter.app')).toBe('gorouter.app')
    expect(hostOf('https://token.vietshare.site/cdx/v1')).toBe('token.vietshare.site')
    expect(hostOf('https://user:pass@api.xpiki.com/v1')).toBe('api.xpiki.com')
    expect(hostOf('api.xpiki.com')).toBe('api.xpiki.com')
  })

  it('lowercases, because hostnames are case-insensitive', () => {
    expect(hostOf('https://API.XPiki.com')).toBe('api.xpiki.com')
  })

  it('returns an empty string for a missing or blank baseUrl', () => {
    expect(hostOf(undefined)).toBe('')
    expect(hostOf('')).toBe('')
    expect(hostOf('   ')).toBe('')
  })
})

describe('vendorKey', () => {
  // Live data ships both spellings of the same gateway. Without this the vendor
  // shows up as two rows and each half reports its own error rate.
  it('collapses the same host written with and without a trailing slash', () => {
    const a = account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/' })
    const b = account({ id: 'b', baseUrl: 'https://kiro.pix4k.com' })
    expect(vendorKey(a)).toBe(vendorKey(b))
  })

  it('keeps different hosts apart even when the provider label is identical', () => {
    const a = account({ id: 'a', baseUrl: 'https://gorouter.app/', provider: 'External OpenAI' })
    const b = account({ id: 'b', baseUrl: 'https://tabitoken.com/', provider: 'External OpenAI' })
    expect(vendorKey(a)).not.toBe(vendorKey(b))
  })

  it('falls back to the provider label when there is no baseUrl', () => {
    const codex = account({ id: 'c', provider: 'OpenAI Codex', authMethod: 'codex' })
    expect(vendorKey(codex)).toBe('provider:OpenAI Codex')
    expect(vendorLabel(codex)).toBe('OpenAI Codex')
  })

  it('never lets a provider label collide with a host key', () => {
    const host = account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/' })
    const named = account({ id: 'b', provider: 'kiro.pix4k.com' })
    expect(vendorKey(host)).not.toBe(vendorKey(named))
  })

  it('uses the host as the display label for a host-grouped vendor', () => {
    expect(vendorLabel(account({ id: 'a', baseUrl: 'https://gorouter.app/' }))).toBe('gorouter.app')
  })

  it('falls back to authMethod and then a constant, so no account lands in an empty bucket', () => {
    expect(vendorLabel(account({ id: 'a', authMethod: 'codex' }))).toBe('codex')
    expect(vendorLabel(account({ id: 'a' }))).toBe('Khác')
  })
})

describe('groupByVendor', () => {
  it('returns one row per vendor with every account in the group', () => {
    const rows = groupByVendor([
      account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/' }),
      account({ id: 'b', baseUrl: 'https://kiro.pix4k.com' }),
      account({ id: 'c', baseUrl: 'https://gorouter.app/' }),
    ], usage(), null)

    expect(rows).toHaveLength(2)
    const kiro = rows.find((row) => row.label === 'kiro.pix4k.com')
    expect(kiro?.accounts.map((a) => a.id).sort()).toEqual(['a', 'b'])
  })

  it('sums the windowed counts of its own accounts only', () => {
    const rows = groupByVendor([
      account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/' }),
      account({ id: 'b', baseUrl: 'https://kiro.pix4k.com' }),
      account({ id: 'c', baseUrl: 'https://gorouter.app/' }),
    ], usage({
      byAccount: {
        a: summary({ requests: 10, errors: 2 }),
        b: summary({ requests: 5, errors: 1 }),
        c: summary({ requests: 900, errors: 900 }),
      },
    }), null)

    const kiro = rows.find((row) => row.label === 'kiro.pix4k.com')!
    expect(kiro.requests).toBe(15)
    expect(kiro.errors).toBe(3)
    const gorouter = rows.find((row) => row.label === 'gorouter.app')!
    expect(gorouter.requests).toBe(900)
    expect(gorouter.errors).toBe(900)
  })

  // An account that served nothing in the period is absent from byAccount, and
  // an older server omits a zero error count. Neither may produce NaN.
  it('reads a missing summary and a missing error count as zero', () => {
    const rows = groupByVendor([
      account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/' }),
      account({ id: 'b', baseUrl: 'https://kiro.pix4k.com', requestCount: 7 }),
    ], usage({ byAccount: { a: summary({ requests: 4 }) } }), null)

    const row = rows[0]
    expect(row.requests).toBe(4)
    expect(row.errors).toBe(0)
    expect(row.lifetimeRequests).toBe(7)
    expect(Number.isNaN(row.errors)).toBe(false)
    expect(vendorErrorRate(row)).toBe(0)
  })

  it('reports no rate rather than zero when the period has no requests', () => {
    const rows = groupByVendor([account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/' })], usage(), null)
    expect(vendorErrorRate(rows[0])).toBeNull()
  })

  it('keeps windowed errors separate from the lifetime counters', () => {
    const rows = groupByVendor([
      account({ id: 'a', baseUrl: 'https://kiro.pix4k.com/', requestCount: 500, errorCount: 40, serviceRequestCount: 5, serviceErrorCount: 1, serviceQuotaErrorCount: 2 }),
    ], usage({ byAccount: { a: summary({ requests: 12, errors: 3, realCost: 0.25 }) } }), null)

    const row = rows[0]
    expect(row.requests).toBe(12)
    expect(row.errors).toBe(3)
    expect(row.lifetimeRequests).toBe(505)
    // All three counters: a quota error is still an error the operator saw.
    expect(row.lifetimeErrors).toBe(43)
    expect(row.realCost).toBe(0.25)
  })

  it('returns an empty array for no accounts', () => {
    expect(groupByVendor([], usage(), null)).toEqual([])
  })

  it('surfaces a cooldown, its reason and the affected models', () => {
    const health: PoolHealth = {
      since: 1_760_000_000,
      accounts: {
        a: { cooldownUntil: 1_760_000_900, cooldownReason: 'auth_failed', consecutiveErrors: 4, modelLocks: { 'qwen3.8-max': { until: 1_760_000_600, reason: 'rate_limited' } } },
      },
    }
    const rows = groupByVendor([
      account({ id: 'a', baseUrl: 'https://gorouter.app/' }),
      account({ id: 'b', baseUrl: 'https://gorouter.app/' }),
    ], usage(), health)

    const row = rows[0]
    expect(row.cooledDown).toBe(1)
    expect(row.cooldownUntil).toBe(1_760_000_900)
    expect(row.cooldownReasons).toEqual(['auth_failed'])
    expect(row.atRisk).toBe(1)
    expect(row.lockedModels).toEqual([{ accountId: 'a', model: 'qwen3.8-max', until: 1_760_000_600, reason: 'rate_limited' }])
  })

  it('tolerates a null health payload', () => {
    const rows = groupByVendor([account({ id: 'a', baseUrl: 'https://gorouter.app/' })], usage(), null)
    expect(rows[0].cooledDown).toBe(0)
    expect(rows[0].lockedModels).toEqual([])
  })

  // The page exists to answer "what is broken". A vendor that is refusing
  // traffic must not be buried under one that merely served the most, even
  // though the busy vendor has more than twice the error rate.
  it('sorts a blocked vendor above a busier one', () => {
    const health: PoolHealth = { since: 1, accounts: { broken: { cooldownUntil: 1_760_000_900, cooldownReason: 'auth_failed' } } }
    const rows = groupByVendor([
      account({ id: 'busy', baseUrl: 'https://busy.example', requestCount: 10_000 }),
      account({ id: 'broken', baseUrl: 'https://broken.example', requestCount: 1 }),
    ], usage({
      byAccount: { busy: summary({ requests: 500, errors: 1 }), broken: summary({ requests: 2, errors: 2 }) },
    }), health)

    expect(rows[0].label).toBe('broken.example')
  })

  it('sorts an erroring vendor above a clean one when neither is blocked', () => {
    const rows = groupByVendor([
      account({ id: 'clean', baseUrl: 'https://clean.example', requestCount: 10_000 }),
      account({ id: 'flaky', baseUrl: 'https://flaky.example', requestCount: 1 }),
    ], usage({
      byAccount: { clean: summary({ requests: 500 }), flaky: summary({ requests: 2, errors: 2 }) },
    }), null)

    expect(rows[0].label).toBe('flaky.example')
  })
})

describe('recentErrors', () => {
  const accounts = [
    account({ id: 'go-1', nickname: 'Gorouter One', baseUrl: 'https://gorouter.app/' }),
    account({ id: 'tab-1', baseUrl: 'https://tabitoken.com/' }),
  ]

  it('keeps failures, drops successes, and preserves newest-first order', () => {
    const rows = recentErrors(accounts, usage({
      recentRequests: [
        { accountId: 'go-1', status: 'error', model: 'qwen3.8-max', error: 'boom', timestamp: '2026-09-15T10:00:00Z' },
        { accountId: 'go-1', status: 'success', model: 'qwen3.8-max', timestamp: '2026-09-15T09:59:00Z' },
        { accountId: 'tab-1', status: 'error', model: 'gpt-5', error: 'nope', timestamp: '2026-09-15T09:58:00Z' },
      ],
    }), 20)

    expect(rows.map((row) => row.error)).toEqual(['boom', 'nope'])
  })

  // The record's own `provider` field is the coarse routing family
  // ("External OpenAI"), shared by every external vendor. Attributing by it
  // would file both of these under one label.
  it('attributes by accountId, ignoring the coarse provider field on the record', () => {
    const rows = recentErrors(accounts, usage({
      recentRequests: [
        { accountId: 'go-1', status: 'error', provider: 'External OpenAI', error: 'a' },
        { accountId: 'tab-1', status: 'error', provider: 'External OpenAI', error: 'b' },
      ],
    }), 20)

    expect(rows[0].vendorLabel).toBe('gorouter.app')
    expect(rows[1].vendorLabel).toBe('tabitoken.com')
    expect(rows[0].accountLabel).toBe('Gorouter One')
  })

  it('caps the list and keeps the most recent entries', () => {
    const records = Array.from({ length: 30 }, (_, i) => ({ accountId: 'go-1', status: 'error', error: `e${i}` }))
    const rows = recentErrors(accounts, usage({ recentRequests: records }), 5)
    expect(rows).toHaveLength(5)
    expect(rows.map((row) => row.error)).toEqual(['e0', 'e1', 'e2', 'e3', 'e4'])
  })

  it('labels a failure whose account is gone instead of dropping it', () => {
    const rows = recentErrors(accounts, usage({
      recentRequests: [{ accountId: 'deleted-9', status: 'error', accountName: 'Old Key', error: 'x' }],
    }), 20)

    expect(rows).toHaveLength(1)
    expect(rows[0].accountLabel).toBe('Old Key')
    expect(rows[0].vendorLabel).toBe('Không rõ tài khoản')
    expect(rows[0].vendorKey).toBe('')
  })

  it('returns an empty array when there is no usage payload', () => {
    expect(recentErrors(accounts, null, 20)).toEqual([])
  })
})
