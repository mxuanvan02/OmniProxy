import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AccountsView } from './AccountsView'
import { api, type Account } from '../lib/api'

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>()
  return {
    ...actual,
    api: {
      ...actual.api,
      cachedAccountModels: vi.fn(),
      accountModels: vi.fn(),
      updateAccount: vi.fn(),
    },
  }
})

const account: Account = {
  id: 'external-1',
  nickname: 'Zen',
  provider: 'external',
  authMethod: 'external-openai',
  baseUrl: 'https://gateway.example/v1',
  enabled: true,
  allowedModels: ['model-a'],
}

describe('AccountsView model restrictions', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(api.cachedAccountModels).mockResolvedValue({ success: true, models: ['model-a', 'model-b'] })
  })

  it('loads only the cached catalog and saves the selected allowlist without a credential', async () => {
    const reload = vi.fn()
    render(<AccountsView accounts={[account]} onReload={reload}/>)

    fireEvent.click(screen.getByRole('button', { name: /Zen/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Sửa cấu hình' }))

    await waitFor(() => expect(api.cachedAccountModels).toHaveBeenCalledWith('external-1'))
    expect(api.accountModels).not.toHaveBeenCalled()
    expect((screen.getByRole('checkbox', { name: 'model-a' }) as HTMLInputElement).checked).toBe(true)

    fireEvent.click(screen.getByRole('checkbox', { name: 'model-b' }))
    fireEvent.click(screen.getByRole('button', { name: 'Lưu thay đổi' }))

    await waitFor(() => expect(api.updateAccount).toHaveBeenCalledWith(
      'external-1',
      expect.objectContaining({ allowedModels: ['model-a', 'model-b'] }),
    ))
    const payload = vi.mocked(api.updateAccount).mock.calls[0][1]
    expect(payload).not.toHaveProperty('accessToken')
    expect(reload).toHaveBeenCalled()
  })

  it('sends an empty allowlist when all models are allowed', async () => {
    render(<AccountsView accounts={[account]} onReload={vi.fn()}/>)

    fireEvent.click(screen.getByRole('button', { name: /Zen/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Sửa cấu hình' }))
    fireEvent.click(screen.getByRole('checkbox', { name: 'Chỉ cho phép các model đã chọn' }))
    fireEvent.click(screen.getByRole('button', { name: 'Lưu thay đổi' }))

    await waitFor(() => expect(api.updateAccount).toHaveBeenCalledWith(
      'external-1',
      expect.objectContaining({ allowedModels: [] }),
    ))
  })
})
