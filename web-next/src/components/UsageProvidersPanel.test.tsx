import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageProvidersPanel } from './UsageProvidersPanel'
import type { RecentRequest, UsageStats } from '../lib/api'

const NOW = new Date('2026-09-15T10:05:00Z').getTime() / 1000

function usage(recs: RecentRequest[]): UsageStats {
  return {
    requests: 100, promptTokens: 1000, completionTokens: 500, totalCost: 0.5,
    totalRequests: 100, totalPromptTokens: 1000, totalCompletionTokens: 500,
    byModel: {}, byAccount: {}, byAPIKey: {}, byEndpoint: {}, recentRequests: recs,
  }
}

describe('UsageProvidersPanel', () => {
  it('includes record 2 min old, excludes 6 min old', () => {
    render(<UsageProvidersPanel usage={usage([
      { timestamp: '2026-09-15T09:58:00Z', accountId: 'old', accountName: 'Old', provider: 'External OpenAI', status: 'ok', inputTokens: 10, outputTokens: 5 },
      { timestamp: '2026-09-15T10:03:00Z', accountId: 'new', accountName: 'New', provider: 'External OpenAI', status: 'ok', inputTokens: 20, outputTokens: 10 },
    ])} now={NOW} />)
    expect(screen.getByText('New')).toBeTruthy()
    expect(screen.queryByText('Old')).toBeNull()
  })

  it('groups by accountId, never by provider', () => {
    render(<UsageProvidersPanel usage={usage([
      { timestamp: '2026-09-15T10:03:00Z', accountId: 'a', accountName: 'Alice', provider: 'External OpenAI', status: 'ok', inputTokens: 10, outputTokens: 5 },
      { timestamp: '2026-09-15T10:04:00Z', accountId: 'b', accountName: 'Bob', provider: 'External OpenAI', status: 'ok', inputTokens: 20, outputTokens: 10 },
    ])} now={NOW} />)
    expect(screen.getByText('Alice')).toBeTruthy()
    expect(screen.getByText('Bob')).toBeTruthy()
    const rows = screen.getAllByRole('row')
    for (const row of rows) {
      expect(row.textContent).not.toContain('External OpenAI')
    }
  })

  it('falls back to accountId.slice(0,8) when accountName absent', () => {
    render(<UsageProvidersPanel usage={usage([
      { timestamp: '2026-09-15T10:03:00Z', accountId: 'abcdefghijk', provider: 'X', status: 'ok', inputTokens: 0, outputTokens: 0 },
    ])} now={NOW} />)
    expect(screen.getByText('abcdefgh')).toBeTruthy()
  })

  it('renders disclosure line', () => {
    render(<UsageProvidersPanel usage={usage([])} now={NOW} />)
    expect(screen.getByText(/bộ đệm 500 request/)).toBeTruthy()
  })

  it('renders empty state', () => {
    render(<UsageProvidersPanel usage={usage([])} now={NOW} />)
    expect(screen.getByText('Chưa có request nào trong 5 phút gần nhất.')).toBeTruthy()
  })

  it('excludes record with unparseable timestamp', () => {
    render(<UsageProvidersPanel usage={usage([
      { timestamp: 'not a date', accountId: 'bad', accountName: 'Bad', provider: 'X', status: 'ok', inputTokens: 0, outputTokens: 0 },
    ])} now={NOW} />)
    expect(screen.queryByText('Bad')).toBeNull()
  })
})
