import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { ModelTestView } from './ModelTestView'

vi.mock('../lib/api', () => ({
  testModelSVG: vi.fn(),
  api: { accounts: vi.fn() },
}))

import { api, testModelSVG, type Account, type SVGTestResult } from '../lib/api'
const mockTest = vi.mocked(testModelSVG)
const mockAccounts = vi.mocked(api.accounts)

const chatAccount: Account = { id: 'a1', nickname: 'VIBE main', provider: 'External OpenAI', enabled: true }
const serviceAccount: Account = { id: 's1', nickname: 'Tavily', provider: 'tavily', enabled: true, capabilities: ['search'] }

beforeEach(() => {
  mockTest.mockReset()
  mockAccounts.mockReset()
  mockAccounts.mockResolvedValue([chatAccount, serviceAccount])
})

describe('ModelTestView', () => {
  const models = ['claude-opus-5', 'gpt-4o', 'qwen3-max']

  it('renders model dropdown with all models', () => {
    render(<ModelTestView models={models} />)
    for (const m of models) {
      expect(screen.getByText(m)).toBeTruthy()
    }
  })

  it('shows empty state when no models', () => {
    render(<ModelTestView models={[]} />)
    expect(screen.getByText('Chưa có model nào')).toBeTruthy()
  })

  it('disables button when no model selected', () => {
    render(<ModelTestView models={[]} />)
    expect((screen.getByRole('button', { name: /chạy svg test/i }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('lists enabled chat accounts and hides service accounts', async () => {
    render(<ModelTestView models={models} />)
    await waitFor(() => {
      expect(screen.getByText(/VIBE main/)).toBeTruthy()
    })
    expect(screen.queryByText(/Tavily/)).toBeNull()
    expect(screen.getByText(/Tự động/)).toBeTruthy()
  })

  it('calls testModelSVG with selected model on click', async () => {
    mockTest.mockResolvedValue({
      success: true, svg: '<svg><circle/></svg>', rawReply: '<svg><circle/></svg>',
      model: 'claude-opus-5', elapsedMs: 1200, tokensUsed: 350,
    })
    render(<ModelTestView models={models} />)
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(mockTest).toHaveBeenCalledWith('claude-opus-5', undefined, undefined)
    })
  })

  it('pins the selected account when one is chosen', async () => {
    mockTest.mockResolvedValue({
      success: true, svg: '<svg/>', rawReply: '<svg/>',
      model: 'claude-opus-5', accountId: 'a1', accountName: 'VIBE main', elapsedMs: 800, tokensUsed: 100,
    })
    render(<ModelTestView models={models} />)
    await waitFor(() => {
      expect(screen.getByText(/VIBE main/)).toBeTruthy()
    })
    fireEvent.change(screen.getByLabelText('Chọn account'), { target: { value: 'a1' } })
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(mockTest).toHaveBeenCalledWith('claude-opus-5', undefined, 'a1')
    })
  })

  it('sends custom prompt when provided', async () => {
    mockTest.mockResolvedValue({
      success: true, svg: '<svg/>', rawReply: '<svg/>',
      model: 'gpt-4o', elapsedMs: 800, tokensUsed: 100,
    })
    render(<ModelTestView models={models} />)
    fireEvent.change(screen.getByPlaceholderText(/prompt tuỳ chỉnh/i), { target: { value: 'Draw a cat' } })
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(mockTest).toHaveBeenCalledWith('claude-opus-5', 'Draw a cat', undefined)
    })
  })

  it('renders SVG container on success', async () => {
    mockTest.mockResolvedValue({
      success: true, svg: '<svg viewBox="0 0 10 10"><rect/></svg>', rawReply: 'raw',
      model: 'claude-opus-5', elapsedMs: 500, tokensUsed: 200,
    })
    render(<ModelTestView models={models} />)
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(screen.getByText('500 ms · 200 tokens')).toBeTruthy()
    })
  })

  it('shows which account served the test', async () => {
    mockTest.mockResolvedValue({
      success: true, svg: '<svg/>', rawReply: '<svg/>',
      model: 'claude-opus-5', accountId: 'a1', accountName: 'VIBE main', elapsedMs: 500, tokensUsed: 200,
    })
    render(<ModelTestView models={models} />)
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(screen.getByText('account: VIBE main')).toBeTruthy()
    })
  })

  it('shows error when result has no SVG', async () => {
    mockTest.mockResolvedValue({
      success: false, svg: '', rawReply: 'I cannot do that',
      model: 'claude-opus-5', elapsedMs: 300, tokensUsed: 50,
      error: 'Model refused',
    })
    render(<ModelTestView models={models} />)
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(screen.getByText('Model refused')).toBeTruthy()
    })
  })

  it('shows network error', async () => {
    mockTest.mockRejectedValue(new Error('Network fail'))
    render(<ModelTestView models={models} />)
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(screen.getByText('Network fail')).toBeTruthy()
    })
  })

  it('shows loading state during request', async () => {
    let resolve: (v: SVGTestResult) => void
    mockTest.mockReturnValue(new Promise((r) => { resolve = r }))
    render(<ModelTestView models={models} />)
    fireEvent.click(screen.getByRole('button', { name: /chạy svg test/i }))
    await waitFor(() => {
      expect(screen.getByText('Đang kiểm tra…')).toBeTruthy()
    })
    resolve!({ success: true, svg: '<svg/>', rawReply: '', model: 'm', elapsedMs: 0, tokensUsed: 0 })
  })
})
