import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsView } from './SettingsView'
import { api } from '../lib/api'

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>()
  return { ...actual, api: { ...actual.api, endpoint: vi.fn(), proxy: vi.fn(), updateSettings: vi.fn(), updateEndpoint: vi.fn(), updateProxy: vi.fn() } }
})

const settings = { requireApiKey: true, port: 8080, host: '127.0.0.1', allowOverUsage: false }

describe('SettingsView mutations', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(api.endpoint).mockResolvedValue({ preferredEndpoint: 'auto', endpointFallback: true })
    vi.mocked(api.proxy).mockResolvedValue({ proxyURL: '' })
  })
  it('loads and saves endpoint and proxy settings with confirmation', async () => {
    const reload = vi.fn()
    render(<SettingsView settings={settings} onReload={reload}/>)
    await screen.findByLabelText('Upstream ưu tiên')
    fireEvent.change(screen.getByLabelText('Upstream ưu tiên'), { target: { value: 'amazonq' } })
    fireEvent.change(screen.getByLabelText('Forward proxy'), { target: { value: 'socks5://127.0.0.1:1080' } })
    fireEvent.click(screen.getByRole('button', { name: 'Lưu routing và proxy' }))
    await waitFor(() => expect(api.updateEndpoint).toHaveBeenCalledWith({ preferredEndpoint: 'amazonq', endpointFallback: true }))
    expect(api.updateProxy).toHaveBeenCalledWith('socks5://127.0.0.1:1080')
    expect(window.confirm).toHaveBeenCalled()
  })
  it('updates API protection without exposing the existing key', async () => {
    render(<SettingsView settings={settings} onReload={vi.fn()}/>)
    fireEvent.click(screen.getByLabelText('Cho phép vượt quota'))
    fireEvent.click(screen.getByRole('button', { name: 'Lưu bảo mật' }))
    await waitFor(() => expect(api.updateSettings).toHaveBeenCalledWith(expect.objectContaining({ allowOverUsage: true })))
    expect(JSON.stringify(vi.mocked(api.updateSettings).mock.calls)).not.toContain('apiKey')
  })
})
