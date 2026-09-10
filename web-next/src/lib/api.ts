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

export function getToken(): string { return sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(TOKEN_KEY) || '' }
function storeToken(token: string, expiresAt: string, remember: boolean) {
  const store = remember ? localStorage : sessionStorage
  store.setItem(TOKEN_KEY, token); if (expiresAt) store.setItem(EXPIRES_KEY, expiresAt)
}
export function clearToken() { for (const s of [sessionStorage, localStorage]) { s.removeItem(TOKEN_KEY); s.removeItem(EXPIRES_KEY) } }
function safeParse(text: string): unknown { try { return JSON.parse(text) } catch { return { raw: text } } }

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  const token = getToken(); if (token) headers.set(TOKEN_HEADER, token)
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const res = await fetch(`/admin/api${path}`, { ...init, headers })
  if (res.status === 401) { clearToken(); throw new ApiError('Phiên đăng nhập đã hết hạn', 401) }
  const text = await res.text(); const body = text ? safeParse(text) : null
  if (!res.ok) {
    const message = body && typeof body === 'object' && 'error' in body && typeof body.error === 'string' ? body.error : `HTTP ${res.status}`
    throw new ApiError(message, res.status)
  }
  return body as T
}

export async function login(password: string, remember: boolean) {
  const res = await fetch('/admin/api/login', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({password}) })
  const body = (safeParse(await res.text()) || {}) as {token?:string; expiresAt?:string; error?:string}
  if (!res.ok || !body.token) throw new ApiError(body.error || `HTTP ${res.status}`, res.status)
  storeToken(body.token, body.expiresAt || '', remember)
}
export async function logout() { try { await request('/logout', {method:'POST'}) } catch {} clearToken() }

export interface Account {
  id:string; email?:string; nickname?:string; provider?:string; providerKind?:string; authMethod?:string; baseUrl?:string; region?:string
  enabled:boolean; banStatus?:string; banReason?:string; weight?:number; allowedModels?:string[]; modelCount?:number; catalogState?:string; catalogSource?:string
  catalogError?:string; catalogCheckedAt?:number; capabilities?:string[]; discoveredCapabilities?:string[]; requestCount?:number; errorCount?:number
  totalTokens?:number; totalCredits?:number; lastUsed?:number; serviceRequestCount?:number; serviceErrorCount?:number; serviceLastUsed?:number
  usagePercent?:number; usageCurrent?:number; usageLimit?:number; nextResetDate?:string; daysRemaining?:number; subscriptionType?:string
  extCreditLimit?:number; extCreditsRemaining?:number; extCreditsUsed?:number; extCreditsCheckedAt?:number; extStatus?:string
  codexPlanType?:string; codexPrimaryUsedPercent?:number; codexSecondaryUsedPercent?:number; codexPrimaryResetAt?:number; codexSecondaryResetAt?:number
  [key:string]: unknown
}
export interface Status { accounts:number; available:number; totalAccounts:number; totalRequests:number; successRequests:number; failedRequests:number; totalTokens:number; totalCredits:number; availableModels:number; modelIds:string[]; uptime:number }
export interface PeriodSummary { requests:number; promptTokens:number; completionTokens:number; realCost?:number; cost?:number; effectiveTokens?:number; cacheReadTokens?:number }
export interface UsageStats extends PeriodSummary { totalRequests:number; totalPromptTokens:number; totalCompletionTokens:number; totalRealCost?:number; totalCost:number; totalEffectiveTokens?:number; totalCacheReadTokens?:number; activeRequests?:Array<{provider:string; model:string; accountId:string}>; recentRequests?:Record<string,unknown>[]; byModel:Record<string,PeriodSummary>; byAccount:Record<string,PeriodSummary>; byAPIKey:Record<string,PeriodSummary>; byEndpoint:Record<string,PeriodSummary>; accountNames?:Record<string,string> }
export interface ChartPoint { label:string; tokens:number; cost:number }
export interface QuotaRow { name:string; used:number; total:number; remaining:number; resetAt?:number; recurring:boolean; unit?:string }
export interface QuotaAccount { id:string; email?:string; nickname?:string; provider:string; providerLabel:string; enabled:boolean; status?:string; banStatus?:string; subscriptionType?:string; quotas?:QuotaRow[]; usagePercent?:number; extCreditsRemaining?:number }
export interface QuotaOverview { providers:Record<string,{provider:string; label:string; accounts:number; activeAccounts:number; usageCurrent:number; usageLimit:number; usagePercent:number}>; accounts:QuotaAccount[]; timestamp:string }
export interface Settings { apiKey?:string; requireApiKey:boolean; port:number; host:string; allowOverUsage:boolean }
export interface Combo { id?:string; name?:string; enabled?:boolean; models?:unknown[]; [key:string]:unknown }

export interface CapabilityProbeResult {
  ok:boolean; status?:number; model?:string; detail?:string; checkedAt?:number; latencyMs?:number; skipped?:boolean; skippedReason?:string
}
export interface CapabilitySummary {
  capability:string; accounts:number; enabledAccounts:number; endpoint?:string; endpoints?:string[]; available:boolean
  verifiedAccounts:number; probeFailures:number; probeSkipped:number; verified:boolean
}
export interface AccountCapability {
  id:string; email?:string; nickname?:string; provider?:string; enabled:boolean; configured?:string[]; discovered?:string[]
  effective?:string[]; discoveredAt?:number; probes?:Record<string,CapabilityProbeResult>; verified?:string[]
}
export interface CapabilityMatrix { capabilities:CapabilitySummary[]; accounts:AccountCapability[] }
export interface CapabilityProbeResponse {
  success:boolean; accountId:string; includeCostly:boolean; probes:Record<string,CapabilityProbeResult>
  verified:string[]; failed:string[]; skipped:string[]; note?:string
}

export const api = {
  accounts: () => request<Account[]>('/accounts'), status: () => request<Status>('/status'), stats: () => request<Status>('/stats'),
  usage: (period='24h') => request<UsageStats>(`/usage/stats?period=${encodeURIComponent(period)}`),
  usageChart: (period='24h') => request<ChartPoint[]>(`/usage/chart?period=${encodeURIComponent(period)}`),
  quota: () => request<QuotaOverview>('/quota/overview'), settings: () => request<Settings>('/settings'),
  logs: () => request<{lines:string[]}>('/logs'), combos: () => request<Combo[]>('/combos'),
  cliStatus: () => request<Record<string,unknown>>('/cli-tools/status'), capabilities: () => request<CapabilityMatrix>('/capabilities'),
  probeCapabilities: (id:string, includeCostly=false) => request<CapabilityProbeResponse>(`/accounts/${encodeURIComponent(id)}/probe-capabilities${includeCostly?'?includeCostly=true':''}`, {method:'POST'}),
  updateAccount: (id:string, patch:Record<string,unknown>) => request(`/accounts/${encodeURIComponent(id)}`, {method:'PUT', body:JSON.stringify(patch)}),
  importExternal: (body:{baseUrl:string;apiKey:string;name:string;test:boolean;authMethod?:string}) => request<{success:boolean;error?:string}>('/auth/external-provider', {method:'POST', body:JSON.stringify(body)}),
  testAccount: (id:string, body:{capability?:string;model?:string;query?:string;url?:string;prompt?:string}) => request<Record<string,unknown>>(`/accounts/${encodeURIComponent(id)}/test`, {method:'POST', body:JSON.stringify(body)}),
  testModel: (model:string) => request<Record<string,unknown>>('/cli-tools/test-model', {method:'POST', body:JSON.stringify({model})}),
  endpoint: () => request<{preferredEndpoint:string;endpointFallback:boolean}>('/endpoint'),
  updateEndpoint: (body:{preferredEndpoint:string;endpointFallback:boolean}) => request('/endpoint', {method:'POST', body:JSON.stringify(body)}),
  proxy: () => request<{proxyURL:string}>('/proxy'),
  updateProxy: (proxyURL:string) => request('/proxy', {method:'POST', body:JSON.stringify({proxyURL})}),
  updateSettings: (body:Partial<Pick<Settings,'requireApiKey'|'allowOverUsage'>> & {apiKey?:string;password?:string}) => request('/settings', {method:'POST', body:JSON.stringify(body)}),
  refreshAccount: (id:string) => request(`/accounts/${encodeURIComponent(id)}/refresh`, {method:'POST'}),
  refreshModels: (id:string) => request(`/accounts/${encodeURIComponent(id)}/models/refresh`, {method:'POST'}),
  refreshCredits: (id:string) => request(`/accounts/${encodeURIComponent(id)}/credits`, {method:'POST'}),
  resetCreditsAvailable: (id:string) => request<{available:number}>(`/accounts/${encodeURIComponent(id)}/reset-credits/available`),
  resetCredits: (id:string) => request(`/accounts/${encodeURIComponent(id)}/reset-credits`, {method:'POST'}),
  cachedAccountModels: (id:string) => request<{success:boolean;models:string[]}>(`/accounts/${encodeURIComponent(id)}/models/cached`),
  accountModels: (id:string) => request<Record<string,unknown>>(`/accounts/${encodeURIComponent(id)}/models`),
  cliToolSettings: (tool:string) => request<{baseUrl?:string;apiKey?:string;model?:string;models?:string[];activeModel?:string;subagentModel?:string;reasoningEffort?:string}>(`/cli-tools/${encodeURIComponent(tool)}`),
  applyCliTool: (tool:string, body:Record<string,unknown>) => request(`/cli-tools/${encodeURIComponent(tool)}`, {method:'POST', body:JSON.stringify(body)}),
}
