import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Shell } from './Shell'

function renderShell(onSection = vi.fn()) {
  render(<Shell section="overview" onSection={onSection} onRefresh={vi.fn()} onLogout={vi.fn()} busy={false} updatedAt={null}>nội dung</Shell>)
  return onSection
}

describe('Shell navigation', () => {
  it('lists the providers section and reports it when clicked', () => {
    const onSection = renderShell()

    fireEvent.click(screen.getByRole('button', { name: /Nhà cung cấp/ }))
    expect(onSection).toHaveBeenCalledWith('providers')
  })

  // The desktop sidebar and the mobile <select> both map the same array, so a
  // missing entry would drop the page from one of them.
  it('offers the section in the mobile selector as well', () => {
    renderShell()
    expect(screen.getByRole('option', { name: 'Nhà cung cấp' })).toBeTruthy()
  })

  it('keeps the existing sections', () => {
    renderShell()
    for (const label of ['Tổng quan', 'Tài khoản', 'Sử dụng', 'Hạn mức', 'API & CLI', 'Thiết lập', 'Nhật ký']) {
      expect(screen.getByRole('option', { name: label })).toBeTruthy()
    }
  })
})
