import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageModelsTable } from './UsageModelsTable'
import type { UsageStats } from '../lib/api'

function usage(overrides: Partial<UsageStats> = {}): UsageStats {
  return {
    requests: 100, promptTokens: 1000, completionTokens: 500, totalCost: 0.5,
    totalRequests: 100, totalPromptTokens: 1000, totalCompletionTokens: 500,
    byModel: {}, byAccount: {}, byAPIKey: {}, byEndpoint: {}, ...overrides,
  }
}

describe('UsageModelsTable', () => {
  it('renders rows ranked by requests descending', () => {
    render(<UsageModelsTable usage={usage({
      byModel: {
        a: { requests: 5, promptTokens: 50, completionTokens: 25 },
        b: { requests: 20, promptTokens: 200, completionTokens: 100 },
      },
    })} />)
    const rows = screen.getAllByRole('row')
    // header + 2 data rows; first data row should be 'b' (higher requests)
    expect(rows[1].textContent).toContain('b')
  })

  it('errors is 0 for summary without errors key', () => {
    render(<UsageModelsTable usage={usage({
      byModel: { a: { requests: 1, promptTokens: 10, completionTokens: 5 } },
    })} />)
    const cells = screen.getAllByRole('cell')
    // errors cell is the 4th column
    expect(cells[3].textContent).toBe('0')
  })

  it('lastCallAt is — when model absent from recentRequests', () => {
    render(<UsageModelsTable usage={usage({
      byModel: { a: { requests: 1, promptTokens: 10, completionTokens: 5 } },
      recentRequests: [],
    })} />)
    const cells = screen.getAllByRole('cell')
    expect(cells[4].textContent).toBe('—')
  })

  it('renders empty state for no models', () => {
    render(<UsageModelsTable usage={usage()} />)
    expect(screen.getByText('Chưa có dữ liệu model trong kỳ.')).toBeTruthy()
  })

  it('never renders NaN', () => {
    const { container } = render(<UsageModelsTable usage={usage({ totalRequests: 0 })} />)
    expect(container.textContent).not.toContain('NaN')
  })
})
