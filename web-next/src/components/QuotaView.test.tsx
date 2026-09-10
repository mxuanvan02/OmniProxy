import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { QuotaView } from './QuotaView'
import { api, type QuotaOverview } from '../lib/api'

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>()
  return { ...actual, api: { ...actual.api, refreshAccount: vi.fn(), refreshCredits: vi.fn(), resetCreditsAvailable: vi.fn(), resetCredits: vi.fn() } }
})

const data: QuotaOverview = {
  providers: {}, timestamp: '2026-09-10T00:00:00Z',
  accounts: [
    { id: 'external-1', nickname: 'Zen', provider: 'external', providerLabel: 'External OpenAI', enabled: true, quotas: [] },
    { id: 'codex-1', nickname: 'Codex', provider: 'codex', providerLabel: 'Codex', enabled: true, quotas: [] },
  ],
}

describe('QuotaView actions', () => {
  beforeEach(() => vi.clearAllMocks())
  it('refreshes quota and reloads the overview', async () => {
    const reload = vi.fn()
    render(<QuotaView data={data} onReload={reload}/>)
    fireEvent.click(screen.getAllByRole('button', { name: 'Làm mới' })[0])
    await waitFor(() => expect(api.refreshAccount).toHaveBeenCalledWith('external-1'))
    expect(reload).toHaveBeenCalled()
  })
  it('checks credits only for an external account', async () => {
    render(<QuotaView data={data} onReload={vi.fn()}/>)
    fireEvent.click(screen.getByRole('button', { name: 'Kiểm tra credit' }))
    await waitFor(() => expect(api.refreshCredits).toHaveBeenCalledWith('external-1'))
  })
  it('checks availability before consuming a Codex bank reset', async () => {
    vi.mocked(api.resetCreditsAvailable).mockResolvedValue({ available: 2 })
    render(<QuotaView data={data} onReload={vi.fn()}/>)
    fireEvent.click(screen.getByRole('button', { name: 'Bank reset' }))
    await waitFor(() => expect(api.resetCredits).toHaveBeenCalledWith('codex-1'))
    expect(window.confirm).toHaveBeenCalled()
  })
})
