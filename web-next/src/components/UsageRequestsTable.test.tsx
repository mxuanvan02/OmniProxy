import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageRequestsTable } from './UsageRequestsTable'
import type { RecentRequest, UsageStats } from '../lib/api'

function usage(recs: RecentRequest[]): UsageStats {
  return {
    requests: 100, promptTokens: 1000, completionTokens: 500, totalCost: 0.5,
    totalRequests: 100, totalPromptTokens: 1000, totalCompletionTokens: 500,
    byModel: {}, byAccount: {}, byAPIKey: {}, byEndpoint: {}, recentRequests: recs,
  }
}

describe('UsageRequestsTable', () => {
  it('renders newest first', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T09:00:00Z', model: 'old', status: 'ok', inputTokens: 10, outputTokens: 5 },
      { timestamp: '2026-09-15T10:00:00Z', model: 'new', status: 'ok', inputTokens: 20, outputTokens: 10 },
    ])} />)
    const rows = screen.getAllByRole('row')
    // First data row should contain 'new'
    expect(rows[1].textContent).toContain('new')
  })

  it('tokenTotal is input + output', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T10:00:00Z', model: 'm', status: 'ok', inputTokens: 100, outputTokens: 50 },
    ])} />)
    expect(screen.getByText('150')).toBeTruthy()
  })

  it('key is — when apiKeyId absent', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T10:00:00Z', model: 'm', status: 'ok', inputTokens: 0, outputTokens: 0 },
    ])} />)
    const cells = screen.getAllByRole('cell')
    expect(cells[1].textContent).toBe('—')
  })

  it('key shows apiKeyId when present', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T10:00:00Z', model: 'm', status: 'ok', inputTokens: 0, outputTokens: 0, apiKeyId: 'key-abc' },
    ])} />)
    expect(screen.getByText('key-abc')).toBeTruthy()
  })

  it('credential falls back to accountId.slice(0,8)', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T10:00:00Z', model: 'm', status: 'ok', inputTokens: 0, outputTokens: 0, accountId: 'abcdefghijk' },
    ])} />)
    expect(screen.getByText('abcdefgh')).toBeTruthy()
  })

  it('failed record shows error text', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T10:00:00Z', model: 'm', status: 'error', error: 'timeout', inputTokens: 0, outputTokens: 0 },
    ])} />)
    expect(screen.getByText('timeout')).toBeTruthy()
  })

  it('cost renders as USD with 4 decimals', () => {
    render(<UsageRequestsTable usage={usage([
      { timestamp: '2026-09-15T10:00:00Z', model: 'm', status: 'ok', inputTokens: 0, outputTokens: 0, realCost: 0.0012 },
    ])} />)
    expect(screen.getByText('$0.0012')).toBeTruthy()
  })

  it('renders empty state', () => {
    render(<UsageRequestsTable usage={usage([])} />)
    expect(screen.getByText('Chưa có request nào.')).toBeTruthy()
  })

  it('never renders NaN or undefined', () => {
    const { container } = render(<UsageRequestsTable usage={usage([])} />)
    expect(container.textContent).not.toContain('NaN')
    expect(container.textContent).not.toContain('undefined')
  })
})
