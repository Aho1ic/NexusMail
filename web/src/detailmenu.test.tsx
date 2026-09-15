import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { MessageDetail } from './components/MessageDetail'
import type { Message, MessageDetails } from './types'

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: 1, account_id: 1, direction: 'incoming', subject: '主题', sender: 'A <a@x.com>',
    recipients: 'me@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'hi',
    body_state: 'ready', body_text: 'hello', received_at: 1767000000000,
    is_read: true, is_starred: false, has_attachments: false, ...overrides,
  }
}

const details: MessageDetails = { message: message(), attachments: [] }

function mount(props: Partial<Parameters<typeof MessageDetail>[0]> = {}) {
  const onNotice = vi.fn()
  render(<MessageDetail
    selected={message()}
    details={details}
    autoLoadRemoteImages={false}
    onBack={() => {}}
    onStar={() => {}}
    onArchive={() => {}}
    onJunk={() => {}}
    onReply={() => {}}
    onNotice={onNotice}
    {...props}
  />)
  return { onNotice }
}

describe('message detail overflow menu', () => {
  afterEach(cleanup)
  beforeEach(() => {
    Object.defineProperty(navigator, 'userAgent', { value: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1.15', configurable: true })
  })

  it('opens a menu next to the star and lists the actions', async () => {
    mount()
    fireEvent.click(screen.getByRole('button', { name: '更多操作' }))
    expect(await screen.findByRole('menu')).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /分享/ })).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /添加到日历/ })).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /导出 PDF/ })).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /一键翻译/ })).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /举报垃圾邮件/ })).toBeInTheDocument()
  })

  it('closes on Escape', async () => {
    mount()
    fireEvent.click(screen.getByRole('button', { name: '更多操作' }))
    await screen.findByRole('menu')
    fireEvent.keyDown(document, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())
  })

  it('shows an error card instead of spinning when the detail fetch failed', () => {
    mount({ details: null, loadError: '网络错误' })
    expect(screen.getByRole('alert')).toHaveTextContent('正文加载失败')
    expect(screen.getByRole('alert')).toHaveTextContent('网络错误')
    expect(document.querySelector('.animate-spin')).toBeNull()
  })
})
