import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageKpiRow } from './UsageKpiRow'
import type { KpiCard } from '../lib/usage'

const CARDS: KpiCard[] = [
  { key: 'a', label: 'Alpha', value: '10', note: 'note a' },
  { key: 'b', label: 'Beta', value: '20', note: 'note b', tone: 'bad' },
  { key: 'c', label: 'Gamma', value: '30', note: 'note c' },
  { key: 'd', label: 'Token hiệu dụng', value: '40', note: 'đã trừ cache' },
]

describe('UsageKpiRow', () => {
  it('renders exactly four cards', () => {
    render(<UsageKpiRow cards={CARDS} />)
    expect(screen.getAllByTestId(/^kpi-/)).toHaveLength(4)
  })

  it('renders each card label and value', () => {
    render(<UsageKpiRow cards={CARDS} />)
    expect(screen.getByText('Alpha')).toBeTruthy()
    expect(screen.getByText('10')).toBeTruthy()
  })

  it('fourth card is effective tokens, not billing', () => {
    render(<UsageKpiRow cards={CARDS} />)
    const fourth = screen.getByTestId('kpi-d')
    expect(fourth.textContent).toContain('hiệu dụng')
    expect(fourth.textContent?.toLowerCase()).not.toContain('hoá đơn')
  })

  it('no card mentions unbilled', () => {
    render(<UsageKpiRow cards={CARDS} />)
    for (const c of CARDS) {
      const el = screen.getByTestId(`kpi-${c.key}`)
      expect(el.textContent?.toLowerCase()).not.toContain('unbilled')
    }
  })

  it('tone bad applies red class', () => {
    render(<UsageKpiRow cards={CARDS} />)
    const badCard = screen.getByTestId('kpi-b')
    expect(badCard.querySelector('.text-red-700')).toBeTruthy()
  })

  it('renders zero state without NaN', () => {
    const empty: KpiCard[] = [
      { key: 'x', label: 'X', value: '0', note: 'empty' },
    ]
    const { container } = render(<UsageKpiRow cards={empty} />)
    expect(container.textContent).not.toContain('NaN')
  })
})
