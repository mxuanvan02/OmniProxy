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
