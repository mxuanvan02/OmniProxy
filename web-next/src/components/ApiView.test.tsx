import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiView } from './ApiView'
import { api } from '../lib/api'

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>()
  return { ...actual, api: { ...actual.api, cliToolSettings: vi.fn(), applyCliTool: vi.fn(), testModel: vi.fn() } }
})

const status = { codex: { installed: true, hasOmniProxy: true } }
const settings = { requireApiKey: true, port: 8080, host: '127.0.0.1', allowOverUsage: false }

describe('ApiView CLI configuration', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(api.cliToolSettings).mockResolvedValue({ baseUrl: 'http://127.0.0.1:8080/v1', model: 'cx/gpt-5.6-sol', reasoningEffort: 'high' })
  })
  it('loads current settings before editing and applies a safe payload', async () => {
    render(<ApiView settings={settings} cliStatus={status} models={['cx/gpt-5.6-sol']}/>)
    fireEvent.click(screen.getByRole('button', { name: /cấu hình codex/i }))
    await waitFor(() => expect(api.cliToolSettings).toHaveBeenCalledWith('codex'))
    fireEvent.change(screen.getByLabelText('Model CLI'), { target: { value: 'cx/gpt-5.6-terra' } })
    fireEvent.click(screen.getByRole('button', { name: 'Áp dụng cấu hình' }))
    await waitFor(() => expect(api.applyCliTool).toHaveBeenCalledWith('codex', expect.objectContaining({ model: 'cx/gpt-5.6-terra' })))
  })
  it('tests the selected model through OmniProxy', async () => {
    render(<ApiView settings={settings} cliStatus={status} models={['cx/gpt-5.6-sol']}/>)
    fireEvent.click(screen.getByRole('button', { name: /cấu hình codex/i }))
    await screen.findByLabelText('Model CLI')
    fireEvent.click(screen.getByRole('button', { name: 'Kiểm thử model' }))
    await waitFor(() => expect(api.testModel).toHaveBeenCalledWith('cx/gpt-5.6-sol'))
  })
})
