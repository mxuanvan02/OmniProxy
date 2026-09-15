# Phase 03 — Frontend data layer

**Priority:** Critical — this is where the vendor-grouping bugs would live.
**Status:** Not started
**Depends on:** phase 01 (`errors` on the wire), phase 02 (`/pool/health` shape)
**Blocks:** phase 04

## Context links

- `plans/20260915-1622-providers-view/design.md` §5
- `web-next/src/lib/api.ts`, `web-next/src/lib/format.ts`
- `web-next/vitest.config.ts`, `web-next/src/test/setup.ts`
- Verified vendor inventory: `plans/20260915-1622-providers-view/plan.md`

## Overview

Every aggregate the page shows is derived client-side from three existing
endpoints plus the new health route. This phase builds that derivation as pure
functions with no React and no fetching, so the grouping rules are testable in
isolation and phase 04 has no decisions left to make.

The reason to isolate this: the three traps in design.md §3 are all attribution
errors that produce a page that looks right and is wrong. They are cheapest to
catch here.

## Key insights

**1. `baseUrl` trailing slashes are inconsistent per vendor, in live data.**
`https://kiro.pix4k.com/` (4 accounts) and `https://kiro.pix4k.com` (2 accounts)
are the same gateway. Same story for `www.sotamodel.net`, `fxqidian.de5.net` and
`api.justwoker.icu`. Without normalisation, five vendors split into ten rows and
each half under-reports its own error rate.

**2. Grouping by host, not by full URL.** `https://token.vietshare.site/cdx/v1`
is the only baseUrl carrying a path today, so host-vs-path grouping is
unobservable in current data. Host is the right choice because the failure this
page exists to surface — "this upstream is sick" — is a property of the host: two
keys against one gateway fail together regardless of path. It also means a future
`https://sotamodel.net/v1` joins the existing `sotamodel.net` row.

**3. The key must be namespaced.** Provider labels are free-form. A host key and
a provider label sharing one keyspace could collide (a provider literally named
after a host), and the two kinds need different display treatment. `host:<host>`
and `provider:<label>` cannot collide.

**4. `recentRequests[].provider` is not the vendor.** Verified:
`proxy/handler.go:4778-4801` sets it from `resolveAccountMeta`, which collapses
every external vendor into `"External OpenAI"` or `"AgentRouter"`. Grouping by
that field would put all thirteen gorouter.app accounts in the same bucket as
tabitoken.com. Attribution must join on `accountId` against the accounts list.

**5. `recentRequests` is untyped today and has no consumer.**
`UsageStats.recentRequests?: Record<string, unknown>[]` is declared in
`api.ts:57` and read by nothing in `web-next` (verified by grep). Typing it is
therefore a safe, self-contained change.

**6. A missing summary key must read as 0, never `NaN`.** `byAccount` is keyed by
account ID, and an account that served nothing in the period is simply absent.
`summary.errors` can also be absent on an older server (`omitempty`, and phase 01
notes pre-upgrade daily buckets carry no error counts). `?? 0` on every read is
the fix, and it is tested.

## Requirements

**Functional**
- `vendorKey(account)` → opaque namespaced key, stable across scheme, trailing
  slash, path and case differences.
- `vendorLabel(account)` → the display name; the bare host for a vendor with a
  `baseUrl`, otherwise the provider label.
- `groupByVendor(accounts, usage, health)` → one row per vendor with windowed and
  lifetime call and error counts, tokens, cost, model count, and live health.
- `recentErrors(accounts, usage, limit)` → newest-first failures attributed to a
  vendor by `accountId`.
- Vendors needing attention sort first.

**Non-functional**
- Pure functions: no React, no fetch, no `api` import.
- No `NaN` or `undefined` reaches a render path.
- Files stay well under 200 lines.

## Architecture

```
GET /admin/api/accounts ─────┐
GET /admin/api/usage/stats ──┼──► groupByVendor() ──► VendorRow[]  ──► ProvidersView
GET /admin/api/pool/health ──┘        │
                                      └─ join on account.id
GET /admin/api/usage/stats ──────────► recentErrors() ──► RecentErrorRow[]
                                          │
                                          └─ join on record.accountId (NOT record.provider)
```

## Related code files

- **Modify:** `web-next/src/lib/api.ts` — `AccountHealth`, `ModelLock`, `PoolHealth`, `RecentRequest`, `PeriodSummary.errors`, `UsageStats.recentRequests` retype, `api.poolHealth`
- **Create:** `web-next/src/lib/providers.ts`
- **Create:** `web-next/src/lib/providers.test.ts`
- **Delete:** none

## Implementation steps

### Step 1: Write the failing tests

Create `web-next/src/lib/providers.test.ts`:

```ts
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
      uptimeSeconds: 600,
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
    const health: PoolHealth = { since: 1, uptimeSeconds: 1, accounts: { broken: { cooldownUntil: 1_760_000_900, cooldownReason: 'auth_failed' } } }
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
```

### Step 2: Run to verify they fail

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- providers.test.ts 2>&1 | tail -20
```

Expected: fails to resolve `./providers`.

### Step 3: Add the API types

In `web-next/src/lib/api.ts`:

**3a.** Add `errors` to `PeriodSummary` (`:56`). Replace that line:

```ts
export interface PeriodSummary { requests:number; promptTokens:number; completionTokens:number; realCost?:number; cost?:number; effectiveTokens?:number; cacheReadTokens?:number; errors?:number }
```

**3b.** Add `RecentRequest` after `PeriodSummary`:

```ts
/** One entry of the tracker's 500-record ring, newest first. `provider` here is
 *  the coarse routing family ("External OpenAI"), shared by every external
 *  vendor — attribute by accountId instead. */
export interface RecentRequest { timestamp?:string; model?:string; provider?:string; accountId?:string; accountName?:string; status?:string; endpoint?:string; error?:string; inputTokens?:number; outputTokens?:number; realCost?:number }
```

**3c.** In `UsageStats` (`:57`), change `recentRequests?:Record<string,unknown>[]` to `recentRequests?:RecentRequest[]`. No other file reads that field (verified by grep), so nothing else needs to change.

**3c-bis.** Add the third lifetime error counter to `Account` (`:49`), after `serviceErrorCount?:number`:

```ts
 serviceQuotaErrorCount?:number
```

The server already publishes it (`proxy/handler.go:8053`) and the interface simply never declared it. Design.md §3 counts it in the lifetime error figure; omitting it would under-report every vendor whose upstream wallet ran dry.

**3d.** Add the pool-health types after `QuotaOverview` (`:61`):

```ts
/** Deadline is unix seconds, not an ISO string: the server sends an int so that
 *  omitempty applies. A zero means "no cooldown". */
export interface ModelLock { until:number; reason?:string }
export interface AccountHealth { cooldownUntil?:number; cooldownReason?:string; consecutiveErrors?:number; modelLocks?:Record<string,ModelLock> }
/** In-memory pool state. Empty after a restart, so `since` is the process start
 *  time and the UI must present this as "since startup", never as history. */
export interface PoolHealth { accounts:Record<string,AccountHealth>; since:number; uptimeSeconds:number }
```

**3e.** In the `api` object (`:82-108`), add after the `quota` line (`:86`):

```ts
  poolHealth: () => request<PoolHealth>('/pool/health'),
```

### Step 4: Write `web-next/src/lib/providers.ts`

```ts
import type { Account, AccountHealth, PeriodSummary, PoolHealth, UsageStats } from './api'
import { accountLabel } from './format'

export interface LockedModel { accountId:string; model:string; until:number; reason:string }

export interface VendorRow {
  key:string
  label:string
  kind:'host'|'provider'
  accounts:Account[]
  lifetimeRequests:number
  lifetimeErrors:number
  requests:number
  errors:number
  promptTokens:number
  completionTokens:number
  realCost:number
  modelCount:number
  cooledDown:number
  cooldownUntil:number
  cooldownReasons:string[]
  atRisk:number
  lockedModels:LockedModel[]
}

export interface RecentErrorRow {
  accountId:string
  accountLabel:string
  vendorKey:string
  vendorLabel:string
  model:string
  error:string
  timestamp:string
}

/** The upstream host, with scheme, userinfo, path, trailing slash and case
 *  removed. Live config ships the same gateway both as "https://x.com/" and
 *  "https://x.com", so without this one vendor becomes two rows.
 *
 *  Grouping stops at the host rather than the full URL on purpose: two keys
 *  against one gateway fail together, which is the failure this page exists to
 *  surface. "https://token.vietshare.site/cdx/v1" is the only path-bearing
 *  baseUrl in the live config, so this is a rule about the future, not a fix. */
export function hostOf(baseUrl:string|undefined):string {
  const raw = baseUrl?.trim()
  if (!raw) return ''
  return raw
    .replace(/^[a-z][a-z0-9+.-]*:\/\//i, '')
    .replace(/^[^/@]*@/, '')
    .split(/[/?#]/)[0]
    .toLowerCase()
}

/** Vendor display name: the bare host for an external provider, otherwise the
 *  provider label. Codex, Antigravity, Gommo and the search services carry no
 *  baseUrl at all, so they fall through to the label. */
export function vendorLabel(a:Account):string {
  const host = hostOf(a.baseUrl)
  if (host) return host
  return a.provider?.trim() || a.providerKind?.trim() || a.authMethod?.trim() || 'Khác'
}

/** Namespaced so a host key can never collide with a provider label, and so the
 *  UI can tell the two kinds apart. */
export function vendorKey(a:Account):string {
  const host = hostOf(a.baseUrl)
  return host ? `host:${host}` : `provider:${vendorLabel(a)}`
}

/** Null, not zero, when the period served nothing — matching format.ts's
 *  errorRate, which reserves null for "chưa có dữ liệu". */
export function vendorErrorRate(row:VendorRow):number|null {
  return row.requests === 0 ? null : row.errors / row.requests
}

function accumulate(row:VendorRow, s:PeriodSummary):void {
  // A key can be absent: an account that served nothing in the period is not in
  // byAccount, and a server older than the errors field omits it entirely.
  row.requests += s.requests ?? 0
  row.errors += s.errors ?? 0
  row.promptTokens += s.promptTokens ?? 0
  row.completionTokens += s.completionTokens ?? 0
  row.realCost += s.realCost ?? 0
}

function absorbHealth(row:VendorRow, accountId:string, h:AccountHealth):void {
  if (h.cooldownUntil) {
    row.cooledDown++
    row.cooldownUntil = Math.max(row.cooldownUntil, h.cooldownUntil)
    const reason = h.cooldownReason
    // Distinct reasons, because a vendor's accounts can be parked for different
    // causes and naming one of them would be a guess.
    if (reason && !row.cooldownReasons.includes(reason)) row.cooldownReasons.push(reason)
  }
  if (h.consecutiveErrors) row.atRisk++
  for (const [model, lock] of Object.entries(h.modelLocks ?? {})) {
    row.lockedModels.push({ accountId, model, until: lock.until, reason: lock.reason ?? '' })
  }
}

/** Higher sorts first. A vendor currently refusing traffic outranks one that
 *  merely failed a lot earlier in the window. */
function attention(row:VendorRow):number {
  return (row.cooledDown > 0 || row.lockedModels.length > 0 ? 2 : 0) + (row.atRisk > 0 ? 1 : 0) + (row.errors > 0 ? 1 : 0)
}

export function groupByVendor(accounts:Account[], usage:UsageStats|null, health:PoolHealth|null):VendorRow[] {
  const byVendor = new Map<string,VendorRow>()
  for (const a of accounts) {
    const key = vendorKey(a)
    let row = byVendor.get(key)
    if (!row) {
      row = {
        key, label: vendorLabel(a), kind: key.startsWith('host:') ? 'host' : 'provider', accounts: [],
        lifetimeRequests: 0, lifetimeErrors: 0, requests: 0, errors: 0,
        promptTokens: 0, completionTokens: 0, realCost: 0, modelCount: 0,
        cooledDown: 0, cooldownUntil: 0, cooldownReasons: [], atRisk: 0, lockedModels: [],
      }
      byVendor.set(key, row)
    }
    row.accounts.push(a)
    row.lifetimeRequests += (a.requestCount ?? 0) + (a.serviceRequestCount ?? 0)
    // Three counters, not two: the server publishes a separate quota-error count
    // (proxy/handler.go:8053) which the TS interface had never declared.
    row.lifetimeErrors += (a.errorCount ?? 0) + (a.serviceErrorCount ?? 0) + (a.serviceQuotaErrorCount ?? 0)
    row.modelCount += a.modelCount ?? 0
    const summary = usage?.byAccount?.[a.id]
    if (summary) accumulate(row, summary)
    const accountHealth = health?.accounts?.[a.id]
    if (accountHealth) absorbHealth(row, a.id, accountHealth)
  }
  return [...byVendor.values()].sort((x, y) =>
    attention(y) - attention(x) ||
    y.errors - x.errors ||
    y.lifetimeRequests - x.lifetimeRequests ||
    x.label.localeCompare(y.label))
}

/** Recent failures, newest first. `usage.recentRequests` is the tracker's
 *  500-record ring, not a time window, so callers must describe it as "n gần
 *  nhất" rather than as a period. */
export function recentErrors(accounts:Account[], usage:UsageStats|null, limit=20):RecentErrorRow[] {
  // Join on accountId. The record's own `provider` field is the coarse routing
  // family shared by every external vendor, so grouping by it would file
  // gorouter.app and tabitoken.com under one label.
  const index = new Map(accounts.map((a) => [a.id, a]))
  const rows:RecentErrorRow[] = []
  for (const rec of usage?.recentRequests ?? []) {
    if (rec.status !== 'error') continue
    const accountId = rec.accountId ?? ''
    const match = index.get(accountId)
    rows.push({
      accountId,
      accountLabel: match ? accountLabel(match) : (rec.accountName || accountId.slice(0, 8) || '—'),
      vendorKey: match ? vendorKey(match) : '',
      vendorLabel: match ? vendorLabel(match) : 'Không rõ tài khoản',
      model: rec.model ?? '',
      error: rec.error ?? '',
      timestamp: rec.timestamp ?? '',
    })
    if (rows.length >= limit) break
  }
  return rows
}
```

### Step 5: Run the tests to verify they pass

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm test -- providers.test.ts 2>&1 | tail -25
```

Expected: all tests pass, 0 failures.

### Step 6: Typecheck

```bash
cd /Users/van/Tools/OmniProxy/web-next
npx tsc -b 2>&1 | tail -20
```

Expected: no output. The `recentRequests` retype is the change most likely to
break something, and this is what proves nothing else read it.

### Step 7: Lint

```bash
cd /Users/van/Tools/OmniProxy/web-next
npm run lint 2>&1 | tail -15
```

Expected: no errors. Warnings about line length or dense expressions are
acceptable; the existing files are written densely.

### Step 8: Commit

```bash
cd /Users/van/Tools/OmniProxy
git add web-next/src/lib/api.ts web-next/src/lib/providers.ts web-next/src/lib/providers.test.ts
git commit -F - <<'EOF'
feat(web-next): derive per-vendor aggregates from the admin API

Every figure the providers page will show is derived client-side, so the
grouping rules live in one pure module with no React and no fetching.

Vendor identity stops at the host. The live config ships the same gateway as
both "https://kiro.pix4k.com/" and "https://kiro.pix4k.com" — four accounts
under one spelling and two under the other — so without normalisation one
upstream becomes two rows and each half reports its own error rate. The same
split affects sotamodel, fxqidian and justwoker.

Recent failures are attributed by accountId, never by the record's own
provider field: that field is the coarse routing family shared by every
external vendor, so gorouter.app and tabitoken.com would collapse into one
label. A missing byAccount entry reads as zero rather than NaN, because an
account that served nothing in the period is simply absent from the map.

Co-Authored-By: Claude Code <noreply@anthropic.com>
EOF
```

## Todo list

- [ ] Step 1: write `providers.test.ts` with the whole suite
- [ ] Step 2: confirm it fails to resolve `./providers`
- [ ] Step 3: add `errors`, `RecentRequest`, the pool-health types and `api.poolHealth`; retype `recentRequests`
- [ ] Step 4: write `providers.ts`
- [ ] Step 5: all tests pass
- [ ] Step 6: `tsc -b` clean
- [ ] Step 7: `npm run lint` clean
- [ ] Step 8: commit

## Success criteria

- `npm test` passes, including the four tests that exist today
  (`AccountsView`, `ApiView`, `LogsView`, `QuotaView`).
- `npx tsc -b` produces no output.
- `groupByVendor` over the live vendor inventory in `plan.md` returns **25**
  rows — 18 hosts plus 7 provider groups — not 29. The four extra rows without
  normalisation are the trailing-slash pairs.
- `vendorErrorRate` returns `null`, never `0`, for a vendor with no requests in
  the period.

## Risk assessment

| Risk | Mitigation |
|---|---|
| `recentRequests` retype breaks a consumer | Verified by grep: no file in `web-next/src` reads it. `tsc -b` in step 6 proves it. |
| `NaN` from an absent summary key | `?? 0` on every field read; two dedicated tests |
| Trailing-slash pairs splitting a vendor | Three `hostOf` assertions plus the live inventory in `plan.md` |
| Grouping by the record's `provider` field | Dedicated test with two records both labelled `External OpenAI` |
| Provider label colliding with a host key | Namespaced keys; dedicated test |
| `Date.now()` in a sort making tests flaky | No time-dependent logic in this module — expiry filtering is the server's job (phase 02) |

## Security considerations

- **No new credential handling.** The module reads only the fields the API
  already returns to an authenticated admin session. It never touches
  `accessToken`, `apiKey` or `extKeyMasked`, and phase 04 asserts none of those
  reach the DOM.
- **No new network calls.** Phase 03 adds one `api.poolHealth()` binding; the
  module itself is pure.
- **No injection surface.** Every value is rendered as text through React, which
  escapes by default. `recentErrors[].error` carries upstream error strings that
  can contain an account email — already returned today by
  `/admin/api/usage/stats`, so this is not new exposure.

## Next steps

Phase 04 renders `VendorRow[]` and `RecentErrorRow[]`. It must:
- label the health panel with `health.since` / `health.uptimeSeconds`;
- state that the error source is a 500-record buffer;
- use `v === '' || v == null ? '—' : v` in its local `KV`, so a vendor with zero
  errors renders "0".
