import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageChartPanel } from './UsageChartPanel'
import type { ChartPoint, UsageStats } from '../lib/api'

function usage(overrides: Partial<UsageStats> = {}): UsageStats {
  return {
    requests: 100, promptTokens: 1000, completionTokens: 500, totalCost: 0.5,
    totalRequests: 100, totalPromptTokens: 1000, totalCompletionTokens: 500,
    byModel: {}, byAccount: {}, byAPIKey: {}, byEndpoint: {}, ...overrides,
  }
}

const CHART: ChartPoint[] = [
  { label: '00:00', tokens: 100, cost: 0.01 },
  { label: '01:00', tokens: 200, cost: 0.02 },
]

describe('UsageChartPanel', () => {
  it('renders dimension tabs with Model pressed by default', () => {
    render(<UsageChartPanel usage={usage()} chart={CHART} />)
    const modelBtn = screen.getByRole('button', { name: 'Model' })
    expect(modelBtn.getAttribute('aria-pressed')).toBe('true')
  })

  it('clicking a dimension tab changes pressed state', () => {
    render(<UsageChartPanel usage={usage({
      byModel: { a: { requests: 1, promptTokens: 10, completionTokens: 5 } },
      byAccount: { acct1: { requests: 1, promptTokens: 20, completionTokens: 10 } },
    })} chart={CHART} />)
    fireEvent.click(screen.getByRole('button', { name: 'Tài khoản' }))
    expect(screen.getByRole('button', { name: 'Tài khoản' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('button', { name: 'Model' }).getAttribute('aria-pressed')).toBe('false')
  })

  it('renders metric toggle with Token pressed by default', () => {
    render(<UsageChartPanel usage={usage()} chart={CHART} />)
    expect(screen.getByRole('button', { name: 'Token' }).getAttribute('aria-pressed')).toBe('true')
  })

  it('switching to Chi phí changes centre total', () => {
    render(<UsageChartPanel usage={usage({
      byModel: { a: { requests: 1, promptTokens: 100, completionTokens: 50, realCost: 0.5 } },
    })} chart={CHART} />)
    fireEvent.click(screen.getByRole('button', { name: 'Chi phí' }))
    expect(screen.getByTestId('donut-total').textContent).toContain('$')
  })

  it('centre total equals sum of slices', () => {
    render(<UsageChartPanel usage={usage({
      byModel: {
        a: { requests: 1, promptTokens: 100, completionTokens: 50 },
        b: { requests: 1, promptTokens: 200, completionTokens: 100 },
      },
    })} chart={CHART} />)
    const totalEl = screen.getByTestId('donut-total')
    expect(totalEl.textContent).toBeTruthy()
  })

  it('renders period failure count from byModel errors', () => {
    render(<UsageChartPanel usage={usage({
      byModel: { a: { requests: 1, promptTokens: 0, completionTokens: 0, errors: 3 } },
    })} chart={CHART} />)
    expect(screen.getByText(/Lỗi trong kỳ/).textContent).toContain('3')
  })

  it('renders empty state when chart is empty', () => {
    render(<UsageChartPanel usage={usage()} chart={[]} />)
    expect(screen.getByText('Chưa có dữ liệu trong kỳ.')).toBeTruthy()
    expect(screen.queryByTestId('chart-area')).toBeNull()
  })

  it('renders empty state when no dimension has positive value', () => {
    render(<UsageChartPanel usage={usage({ byModel: {} })} chart={CHART} />)
    expect(screen.getByText('Kỳ này chưa có lưu lượng để tính cơ cấu.')).toBeTruthy()
  })

  it('never renders NaN for zero-total usage', () => {
    const { container } = render(<UsageChartPanel usage={usage({ totalRequests: 0 })} chart={[]} />)
    expect(container.textContent).not.toContain('NaN')
  })
})
