import { act, fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LogsView } from './LogsView'

class MockEventSource {
  static instances: MockEventSource[] = []
  url: string
  onmessage: ((event: MessageEvent) => void) | null = null
  onerror: (() => void) | null = null
  onopen: (() => void) | null = null
  close = vi.fn()
  constructor(url: string | URL) { this.url = String(url); MockEventSource.instances.push(this) }
}

describe('LogsView live SSE', () => {
  beforeEach(() => { MockEventSource.instances = []; sessionStorage.setItem('admin_token', 'secret token'); vi.stubGlobal('EventSource', MockEventSource) })
  it('opens an authenticated bounded stream and appends lines', () => {
    render(<LogsView initialLines={['snapshot']}/>)
    const source = MockEventSource.instances[0]
    const url = new URL(source.url, 'http://localhost')
    expect(url.searchParams.get('token')).toBe('secret token')
    expect(url.searchParams.get('tail')).toBe('1000')
    act(() => source.onmessage?.({ data: JSON.stringify({ line: 'live error' }), lastEventId: '42' } as MessageEvent))
    expect(screen.getByText('live error')).toBeTruthy()
  })
  it('pauses, clears and resumes with the last event cursor', () => {
    render(<LogsView initialLines={['snapshot']}/>)
    const source = MockEventSource.instances[0]
    fireEvent.click(screen.getByRole('button', { name: 'Tạm dừng' }))
    act(() => source.onmessage?.({ data: JSON.stringify({ line: 'ignored' }), lastEventId: '7' } as MessageEvent))
    expect(screen.queryByText('ignored')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Xóa màn hình' }))
    expect(screen.queryByText('snapshot')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Tiếp tục' }))
    expect(MockEventSource.instances.at(-1)?.url).toContain('lastEventId=7')
  })
})
