// Thin client for the existing OmniProxy admin API.
//
// Contract confirmed from proxy/handler.go + proxy/admin_session.go:
//   POST /admin/api/login   {password}      -> {token, expiresAt}
//   POST /admin/api/logout                  -> revokes the token
//   GET  /admin/api/accounts                -> Account[] (ETag revalidated)
//   GET  /admin/api/stats                   -> aggregate counters
// Every route except /login requires the X-Admin-Token header.

const TOKEN_HEADER = 'X-Admin-Token'
const TOKEN_KEY = 'admin_token'
const EXPIRES_KEY = 'admin_expires_at'

export class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export function getToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(TOKEN_KEY) || ''
}

function storeToken(token: string, expiresAt: string, remember: boolean): void {
  const store = remember ? localStorage : sessionStorage
  store.setItem(TOKEN_KEY, token)
  if (expiresAt) store.setItem(EXPIRES_KEY, expiresAt)
}

export function clearToken(): void {
  for (const store of [sessionStorage, localStorage]) {
    store.removeItem(TOKEN_KEY)
    store.removeItem(EXPIRES_KEY)
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = getToken()
  const headers = new Headers(init.headers)
  if (token) headers.set(TOKEN_HEADER, token)
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const res = await fetch(`/admin/api${path}`, { ...init, headers })

  if (res.status === 401) {
    clearToken()
    throw new ApiError('Phiên đăng nhập đã hết hạn', 401)
  }

  const text = await res.text()
  const body: unknown = text ? safeParse(text) : null

  if (!res.ok) {
    const message =
      (body && typeof body === 'object' && 'error' in body && typeof body.error === 'string'
        ? body.error
        : '') || `HTTP ${res.status}`
    throw new ApiError(message, res.status)
  }

  return body as T
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return { raw: text }
  }
}

export async function login(password: string, remember: boolean): Promise<void> {
  const res = await fetch('/admin/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  })
  const text = await res.text()
  const body = (text ? safeParse(text) : {}) as { token?: string; expiresAt?: string; error?: string }
  if (!res.ok || !body.token) {
    throw new ApiError(body.error || `HTTP ${res.status}`, res.status)
  }
  storeToken(body.token, body.expiresAt ?? '', remember)
}

export async function logout(): Promise<void> {
  try {
    await request('/logout', { method: 'POST' })
  } catch {
    // A failed revoke must not trap the operator in a logged-in shell.
  }
  clearToken()
}

export const api = {
  accounts: () => request<Account[]>('/accounts'),
  stats: () => request<Record<string, unknown>>('/stats'),
}

/** Subset of /admin/api/accounts that this UI reads. The endpoint returns ~70
 *  fields; unlisted ones are intentionally ignored rather than mistyped. */
export interface Account {
  id: string
  email?: string
  nickname?: string
  provider?: string
  providerKind?: string
  authMethod?: string
  baseUrl?: string
  region?: string
  enabled: boolean
  banStatus?: string
  banReason?: string
  weight?: number
  modelCount?: number
  catalogState?: string
  catalogSource?: string
  catalogError?: string
  catalogCheckedAt?: number
  capabilities?: string[]
  requestCount?: number
  errorCount?: number
  totalTokens?: number
  totalCredits?: number
  lastUsed?: number
  serviceRequestCount?: number
  serviceErrorCount?: number
  serviceLastUsed?: number
  usagePercent?: number
  daysRemaining?: number
  subscriptionType?: string
  extCreditLimit?: number
  extCreditsRemaining?: number
  extCreditsUsed?: number
  extCreditsCheckedAt?: number
}
