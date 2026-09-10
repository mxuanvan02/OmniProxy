import type { Account } from './api'

export function accountLabel(a: Account): string {
  return a.nickname?.trim() || a.email?.trim() || a.id.slice(0, 8)
}

export function providerLabel(a: Account): string {
  const kind = a.providerKind?.trim()
  const provider = a.provider?.trim()
  if (provider && kind && provider !== kind) return `${provider} · ${kind}`
  return provider || kind || '—'
}

export function compactNumber(value: number | undefined): string {
  if (!value) return '0'
  if (value < 1000) return String(value)
  return new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 }).format(value)
}

/** Full grouped value for tooltips. compactNumber rounds to one decimal, so
 *  "1.3B" hides up to 50M — the exact figure has to stay reachable somewhere. */
export function exactNumber(value: number | undefined): string {
  return new Intl.NumberFormat('vi-VN').format(value ?? 0)
}

export function relativeTime(unixSeconds: number | undefined): string {
  if (!unixSeconds) return '—'
  const deltaSeconds = Math.round(unixSeconds - Date.now() / 1000)
  const abs = Math.abs(deltaSeconds)
  const fmt = new Intl.RelativeTimeFormat('vi', { numeric: 'auto' })
  if (abs < 60) return fmt.format(deltaSeconds, 'second')
  if (abs < 3600) return fmt.format(Math.round(deltaSeconds / 60), 'minute')
  if (abs < 86_400) return fmt.format(Math.round(deltaSeconds / 3600), 'hour')
  return fmt.format(Math.round(deltaSeconds / 86_400), 'day')
}

export type Health = 'active' | 'idle' | 'disabled' | 'banned'

/** Health is derived, not stored: an account can be enabled yet banned, or
 *  disabled yet still carry historical counters. Ban outranks enablement. */
export function health(a: Account): Health {
  const ban = a.banStatus?.toUpperCase()
  if (ban && ban !== 'ACTIVE' && ban !== '') return 'banned'
  if (!a.enabled) return 'disabled'
  const requests = (a.requestCount ?? 0) + (a.serviceRequestCount ?? 0)
  return requests > 0 ? 'active' : 'idle'
}

export function errorRate(a: Account): number | null {
  const requests = (a.requestCount ?? 0) + (a.serviceRequestCount ?? 0)
  if (requests === 0) return null
  const errors = (a.errorCount ?? 0) + (a.serviceErrorCount ?? 0)
  return errors / requests
}
