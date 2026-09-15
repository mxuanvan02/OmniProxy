import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageHero } from './UsageHero'
import type { HeroStats } from '../lib/usage'

function stats(overrides: Partial<HeroStats> = {}): HeroStats {
  return {
    successTokens: 1_530_000_000,
    totalRequests: 100,
    failedRequests: 5,
    lastCallAt: null,
    activeRequests: 0,
    activeCredentials: 2,
    totalCredentials: 5,
    modelCount: 12,
    ...overrides,
  }
}

describe('UsageHero', () => {
  it('renders compact figure with exact value in title', () => {
    render(<UsageHero stats={stats()} totalRealCost={0.5} totalCost={0.3} />)
    const el = screen.getByText('1.5B')
    expect(el.getAttribute('title')).toBe('1.530.000.000')
  })

  it('renders each sub-stat', () => {
    render(<UsageHero stats={stats()} totalRealCost={0} totalCost={0} />)
    expect(screen.getByText(/Request:/)).toBeTruthy()
    expect(screen.getByText(/Lỗi:/)).toBeTruthy()
    expect(screen.getByText(/Tài khoản:/)).toBeTruthy()
    expect(screen.getByText(/Model:/)).toBeTruthy()
  })

  it('renders — for null lastCallAt', () => {
    render(<UsageHero stats={stats({ lastCallAt: null })} totalRealCost={0} totalCost={0} />)
    expect(screen.getByText('—')).toBeTruthy()
  })

  it('renders serving wording when activeRequests > 0', () => {
    render(<UsageHero stats={stats({ activeRequests: 1 })} totalRealCost={0} totalCost={0} />)
    expect(screen.getByText('Đang phục vụ')).toBeTruthy()
  })

  it('renders idle wording when activeRequests is 0', () => {
    render(<UsageHero stats={stats({ activeRequests: 0 })} totalRealCost={0} totalCost={0} />)
    expect(screen.getByText('Chưa có request đang chạy')).toBeTruthy()
  })

  it('renders coherent zero state without NaN or undefined', () => {
    const zero: HeroStats = {
      successTokens: 0, totalRequests: 0, failedRequests: 0, lastCallAt: null,
      activeRequests: 0, activeCredentials: 0, totalCredentials: 0, modelCount: 0,
    }
    const { container } = render(<UsageHero stats={zero} totalRealCost={0} totalCost={0} />)
    expect(container.textContent).not.toContain('NaN')
    expect(container.textContent).not.toContain('undefined')
  })

  it('renders headline cost as USD', () => {
    render(<UsageHero stats={stats()} totalRealCost={1.234} totalCost={0.567} />)
    expect(screen.getByText('$1.234')).toBeTruthy()
  })
})
