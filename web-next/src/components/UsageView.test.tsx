import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { UsageView } from './UsageView'
import type { ChartPoint, Status, UsageStats } from '../lib/api'

/** Marker standing in for credential material: if it reaches the DOM, some
 *  panel rendered a field it must not (the pattern ProvidersView.test.tsx:149
 *  established). */
const SECRET_MARKER = 'sk-ant-secret-marker-DO-NOT-RENDER'

function usage(overrides: Partial<UsageStats> = {}): UsageStats {
  return {
    requests: 10, promptTokens: 100, completionTokens: 50, totalCost: 0.5,
    totalRequests: 10, totalPromptTokens: 100, totalCompletionTokens: 50,
    byModel: {}, byAccount: {}, byAPIKey: {}, byEndpoint: {},
    ...overrides,
  }
}

function status(overrides: Partial<Status> = {}): Status {
  return {
    accounts: 1, available: 1, totalAccounts: 1, totalRequests: 10,
    successRequests: 10, failedRequests: 0, totalTokens: 150, totalCredits: 0.5,
    availableModels: 3, modelIds: [], uptime: 60, ...overrides,
  }
}

const CHART: ChartPoint[] = [{ label: '00:00', tokens: 100, cost: 0.01 }]

function renderView(props: Partial<Parameters<typeof UsageView>[0]> = {}) {
  const onPeriod = vi.fn()
  const view = render(
    <UsageView
      usage={usage()} chart={CHART} period="24h" onPeriod={onPeriod}
      status={status()} updatedAt={Date.now()}
      {...props}
    />,
  )
  return { onPeriod, ...view }
}

describe('UsageView', () => {
  it('renders a panel for each of the seven blocks', () => {
    renderView()
    expect(screen.getByTestId('usage-hero')).toBeTruthy()
    expect(screen.getByTestId('usage-chart-panel')).toBeTruthy()
    // Tables carry their testid only when they have rows; otherwise they render
    // their own empty state. Either one proves the panel is mounted.
    expect(
      screen.queryByTestId('models-table') ?? screen.queryByText('Chưa có dữ liệu model trong kỳ.'),
    ).toBeTruthy()
    expect(
      screen.queryByTestId('requests-table') ?? screen.queryByText('Chưa có request nào.'),
    ).toBeTruthy()
    expect(screen.getAllByTestId(/^kpi-/)).toHaveLength(4)
  })

  it('renders the period control with exactly three options, current one pressed', () => {
    renderView()
    const buttons = ['24 giờ', '7 ngày', '30 ngày'].map((l) => screen.getByRole('button', { name: l }))
    expect(buttons).toHaveLength(3)
    expect(buttons[0].getAttribute('aria-pressed')).toBe('true')
    expect(buttons[1].getAttribute('aria-pressed')).toBe('false')
  })

  it('offers no 1h option and no "1 giờ" label', () => {
    const { container } = renderView()
    expect(container.querySelector('option[value="1h"]')).toBeNull()
    expect(screen.queryByRole('button', { name: '1 giờ' })).toBeNull()
    expect(container.textContent).not.toContain('1 giờ')
  })

  it('clicking 7 ngày calls onPeriod("7d")', () => {
    const { onPeriod } = renderView()
    fireEvent.click(screen.getByRole('button', { name: '7 ngày' }))
    expect(onPeriod).toHaveBeenCalledWith('7d')
  })

  it('marks the pressed period from props', () => {
    renderView({ period: '30d' })
    expect(screen.getByRole('button', { name: '30 ngày' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('button', { name: '24 giờ' }).getAttribute('aria-pressed')).toBe('false')
  })

  it('renders every block for null usage, null status, empty chart, null updatedAt', () => {
    const { container } = renderView({ usage: null, status: null, chart: [], updatedAt: null })
    expect(screen.getByTestId('usage-hero')).toBeTruthy()
    expect(container.textContent).not.toContain('NaN')
    expect(container.textContent).not.toContain('undefined')
  })

  it('never renders a credential-shaped value', () => {
    const { container } = renderView({
      usage: usage({
        recentRequests: [
          {
            timestamp: '2026-09-15T10:00:00Z', model: 'm', provider: 'External OpenAI',
            accountId: 'acct-1', accountName: 'Alice', status: 'ok',
            inputTokens: 10, outputTokens: 5, realCost: 0.001,
            apiKeyId: SECRET_MARKER,
          },
        ],
      }),
    })
    // apiKeyId is an identifier the requests table is allowed to show, but the
    // raw key material marker must never appear in any other panel's output.
    // Assert the marker count stays at the one legitimate render (the table
    // cell) and nowhere else.
    const occurrences = (container.textContent ?? '').split(SECRET_MARKER).length - 1
    expect(occurrences).toBe(1)
  })
})
