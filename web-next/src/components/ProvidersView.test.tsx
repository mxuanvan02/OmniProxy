import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ProvidersView } from './ProvidersView'
import type { Account, PeriodSummary, PoolHealth, UsageStats } from '../lib/api'

function account(over: Partial<Account> & { id: string }): Account {
  return { enabled: true, ...over }
}

function summary(over: Partial<PeriodSummary> = {}): PeriodSummary {
  return { requests: 0, promptTokens: 0, completionTokens: 0, ...over }
}

// Two accounts on one host with the same provider label ("External OpenAI"
// collapses every external vendor), one account on a second host, one whose
// catalog refresh just failed, and one with no baseUrl at all — the five shapes
// the page has to keep apart.
const accounts: Account[] = [
  account({ id: 'go-1', nickname: 'Gorouter One', baseUrl: 'https://gorouter.app/', provider: 'External OpenAI', requestCount: 900 }),
  account({ id: 'go-2', baseUrl: 'https://gorouter.app/', provider: 'External OpenAI', requestCount: 100 }),
  account({ id: 'tab-1', nickname: 'Tab One', baseUrl: 'https://tabitoken.com/', provider: 'External OpenAI', requestCount: 40, modelCount: 12, catalogState: 'ready', catalogSource: 'builtin' }),
  // Still serving its last-known models, but the refresh that would confirm them
  // failed — the case modelCount alone cannot express.
  account({ id: 'fx-1', nickname: 'Fx One', baseUrl: 'https://fxqidian.de5.net/', modelCount: 3, catalogState: 'stale', catalogError: 'loadCodeAssist: HTTP 401: invalid metadata.platform' }),
  account({ id: 'codex-1', nickname: 'Codex Key', provider: 'OpenAI Codex', authMethod: 'codex' }),
]

const usage: UsageStats = {
  requests: 0, promptTokens: 0, completionTokens: 0, cost: 0,
  totalRequests: 0, totalPromptTokens: 0, totalCompletionTokens: 0, totalCost: 0,
  byModel: {}, byAPIKey: {}, byEndpoint: {},
  byAccount: {
    'go-1': summary({ requests: 30, errors: 6 }),
    // tab-1 served requests but has no error count: a server older than the
    // errors field omits a zero. It must render "0", never "—".
    'tab-1': summary({ requests: 10 }),
    'codex-1': summary({ requests: 4, errors: 1 }),
  },
  recentRequests: [
    { accountId: 'tab-1', status: 'error', provider: 'External OpenAI', model: 'gpt-5', error: 'upstream 500', timestamp: '2026-09-15T10:00:00Z' },
    { accountId: 'go-1', status: 'success', provider: 'External OpenAI', model: 'qwen3.8-max' },
  ],
}

const coolingPool: PoolHealth = {
  since: 1_760_000_000,
  accounts: {
    'go-1': { cooldownUntil: 1_760_000_900, cooldownReason: 'auth_failed', consecutiveErrors: 4, modelLocks: { 'qwen3.8-max': { until: 1_760_000_600, reason: 'rate_limited' } } },
  },
}

const props = { accounts, usage, pool: null, period: '24h', onPeriod: () => {}, onReload: () => {} }

describe('ProvidersView', () => {
  it('renders one card per vendor, grouped by host', () => {
    render(<ProvidersView {...props} />)

    expect(within(screen.getByTestId('vendor-host:gorouter.app')).getByText(/2 tài khoản/)).toBeTruthy()
    expect(within(screen.getByTestId('vendor-host:tabitoken.com')).getByText(/1 tài khoản/)).toBeTruthy()
    // No baseUrl, so it groups by provider label instead of landing in an
    // empty-string bucket.
    expect(screen.getByTestId('vendor-provider:OpenAI Codex')).toBeTruthy()
  })

  it('shows the vendor API link', () => {
    render(<ProvidersView {...props} />)
    expect(within(screen.getByTestId('vendor-host:gorouter.app')).getByText('https://gorouter.app/')).toBeTruthy()
  })

  // The falsy trap: AccountsView's KV renders String(v||'—'), which turns a
  // legitimate zero into an em dash. On this page that reads as "no data".
  // Asserted against the dd that follows each label, so a legitimate '—' in some
  // other row cannot mask the bug.
  it('renders a zero error count as "0", not as an em dash', () => {
    render(<ProvidersView {...props} />)
    const card = screen.getByTestId('vendor-host:tabitoken.com')

    expect(within(card).getByText('Lỗi (kỳ)').nextElementSibling?.textContent).toBe('0')
    expect(within(card).getByText('Lỗi (luỹ kế)').nextElementSibling?.textContent).toBe('0')
  })

  it('sums the period counts of the accounts in the group', () => {
    render(<ProvidersView {...props} />)
    const card = screen.getByTestId('vendor-host:gorouter.app')

    // go-1: 30 requests / 6 errors. go-2 served nothing, so it is absent from
    // byAccount and contributes zero rather than NaN.
    expect(within(card).getByText('30')).toBeTruthy()
    expect(within(card).getByText('6')).toBeTruthy()
    expect(within(card).getByText('20.0%')).toBeTruthy()
  })

  // The API publishes five separate catalog fields because modelCount alone
  // cannot tell "no chat catalog" from "credential just died" from "never
  // probed" (proxy/handler.go:7991-7994). The card shows the count *and* the
  // state, and repeats the error verbatim — a truncated upstream message is
  // still the most useful thing on the card.
  it('shows catalog health: the count with its state, and the error verbatim', () => {
    render(<ProvidersView {...props} />)

    expect(within(screen.getByTestId('vendor-host:tabitoken.com')).getByText('12 · ready')).toBeTruthy()

    const broken = within(screen.getByTestId('vendor-host:fxqidian.de5.net'))
    expect(broken.getByText('3 · lỗi catalog')).toBeTruthy()
    expect(broken.getByText('loadCodeAssist: HTTP 401: invalid metadata.platform')).toBeTruthy()

    // codex-1 has no catalog fields at all: no count, so no state is claimed.
    expect(within(screen.getByTestId('vendor-provider:OpenAI Codex')).getByText('Chưa nạp')).toBeTruthy()
  })

  it('calls onPeriod with the selected period', () => {
    const onPeriod = vi.fn()
    render(<ProvidersView {...props} onPeriod={onPeriod} />)

    fireEvent.click(screen.getByRole('button', { name: '7 ngày' }))
    expect(onPeriod).toHaveBeenCalledWith('7d')
  })

  it('marks a vendor whose account is in cooldown, with the reason', () => {
    render(<ProvidersView {...props} pool={coolingPool} />)
    const card = screen.getByTestId('vendor-host:gorouter.app')

    expect(within(card).getByText('Đang bị chặn')).toBeTruthy()
    expect(within(card).getByText(/Sai hoặc hết hạn xác thực/)).toBeTruthy()
    expect(within(card).getByText(/qwen3\.8-max/)).toBeTruthy()
  })

  // The record's own `provider` field says "External OpenAI" for every external
  // vendor, so attributing by it would file this failure under gorouter.app.
  it('attributes a recent failure by accountId, not by the record provider label', () => {
    render(<ProvidersView {...props} />)
    const panel = screen.getByText('Lỗi gần đây').closest('section')!

    expect(within(panel).getByText('Tab One')).toBeTruthy()
    expect(within(panel).getByText('tabitoken.com')).toBeTruthy()
    expect(within(panel).getByText('upstream 500')).toBeTruthy()
    expect(within(panel).queryByText('External OpenAI')).toBeNull()
  })

  it('filters vendors by label and by account name', () => {
    render(<ProvidersView {...props} />)
    const search = screen.getByLabelText('Tìm nhà cung cấp')

    fireEvent.change(search, { target: { value: 'gorouter one' } })
    expect(screen.getByTestId('vendor-host:gorouter.app')).toBeTruthy()
    expect(screen.queryByTestId('vendor-host:tabitoken.com')).toBeNull()
  })

  it('never renders a credential-shaped value', () => {
    // The markers are short and deliberately unshaped: the assertion is that no
    // credential-bearing *field* reaches the DOM, and a realistic-looking
    // literal here would only trip the repo's pre-commit secret scanner.
    const withSecrets = [account({ id: 'go-1', baseUrl: 'https://gorouter.app/', accessToken: 'tok-1-LEAK', apiKey: 'key-1-LEAK', extKeyMasked: 'msk-1-LEAK' })]
    render(<ProvidersView {...props} accounts={withSecrets} />)

    expect(document.body.textContent).not.toContain('LEAK')
  })

  it('shows the empty state when nothing failed', () => {
    render(<ProvidersView {...props} usage={{ ...usage, recentRequests: [] }} />)
    expect(screen.getByText('Chưa ghi nhận lỗi nào trong bộ đệm.')).toBeTruthy()
  })
})
