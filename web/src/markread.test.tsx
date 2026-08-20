import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

class FakeSocket {
  static instances: FakeSocket[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  constructor(public url: string) { FakeSocket.instances.push(this) }
  close() { /* no-op */ }
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'mail@example.com', display_name: 'Mail', provider: 'qq', status: 'connected' }
const mailbox = { id: 5, account_id: 1, remote_name: 'Archive', display_name: '归档', role: 'archive', sync_mode: 'lazy' }

function message(id: number, subject: string, read: boolean) {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'preview',
    body_state: 'ready', received_at: 0, is_read: read, is_starred: false, has_attachments: false,
  }
}

describe('mark the current view read', () => {
  afterEach(cleanup)

  let inbox = [message(1, 'First mail', false), message(2, 'Second mail', false)]
  let markRead: { updated: number; capped?: boolean; partial?: boolean }
  let calls: string[]

  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    inbox = [message(1, 'First mail', false), message(2, 'Second mail', false)]
    markRead = { updated: 2 }
    calls = []
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      calls.push(`${(init?.method ?? 'GET').toUpperCase()} ${url}`)
      if (url === '/api/v1/accounts') return json({ items: [account] })
      if (url.includes('/mailboxes')) return json({ items: [mailbox] })
      if (url.startsWith('/api/v1/messages/mark-read')) {
        // The provider is the source of truth, so the next feed read reflects it.
        inbox = inbox.map(item => ({ ...item, is_read: true }))
        return json(markRead)
      }
      if (url.startsWith('/api/v1/messages')) return json({ items: inbox })
      return json({})
    }))
  })

  async function mount() {
    render(<App />)
    expect(await screen.findByRole('button', { name: /First mail/ })).toBeInTheDocument()
  }

  // Compares scopes by decoded parameters instead of by URL text: the order the
  // caller happens to append them in is not part of the contract.
  function scopeOf(method: string, path: string) {
    const call = calls.filter(entry => entry.startsWith(`${method} ${path}?`) || entry === `${method} ${path}`).pop()
    if (!call) return null
    return Object.fromEntries(new URLSearchParams(call.slice(`${method} ${path}`.length + 1)))
  }

  it('posts the same scope the feed is showing and reports the count', async () => {
    await mount()

    fireEvent.click(screen.getByRole('button', { name: '全部已读' }))

    await waitFor(() => expect(calls).toContain('POST /api/v1/messages/mark-read?folder=inbox'))
    expect(await screen.findByRole('status')).toHaveTextContent('已标记 2 封为已读')
  })

  it('scopes the request to the selected mailbox rather than the whole inbox', async () => {
    await mount()
    fireEvent.click(screen.getAllByRole('button', { name: /mail@example\.com/ })[0])
    fireEvent.click(await screen.findByRole('button', { name: '归档' }))

    fireEvent.click(screen.getByRole('button', { name: '全部已读' }))

    await waitFor(() => expect(calls).toContain('POST /api/v1/messages/mark-read?account_id=1&mailbox_id=5'))
  })

  it('reloads the feed so the unread badges cannot go stale', async () => {
    await mount()
    // The sidebar count comes from the loaded page, not from the response.
    expect(screen.getByText('2')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '全部已读' }))

    await waitFor(() => expect(screen.queryByText('2')).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: '全部已读' })).toBeDisabled()
  })

  it('says more unread mail remains when the server capped the pass', async () => {
    markRead = { updated: 2000, capped: true }
    await mount()

    fireEvent.click(screen.getByRole('button', { name: '全部已读' }))

    expect(await screen.findByRole('status')).toHaveTextContent('已标记 2000 封，仍有未读邮件，可再次点击')
  })

  it('says which part failed when only some accounts accepted the flag', async () => {
    markRead = { updated: 1, partial: true }
    await mount()

    fireEvent.click(screen.getByRole('button', { name: '全部已读' }))

    expect(await screen.findByRole('status')).toHaveTextContent('已标记 1 封，部分账户同步失败')
  })

  it('carries the active search so the filter cannot be marked past', async () => {
    await mount()
    fireEvent.change(screen.getByPlaceholderText('搜索主题、发件人或正文…'), { target: { value: '验证码' } })
    // The feed debounces, and the button must wait for the same term it shows.
    await waitFor(() => expect(scopeOf('GET', '/api/v1/messages')).toEqual({ folder: 'inbox', limit: '40', query: '验证码' }))

    fireEvent.click(screen.getByRole('button', { name: '全部已读' }))

    await waitFor(() => expect(scopeOf('POST', '/api/v1/messages/mark-read')).toEqual({ folder: 'inbox', query: '验证码' }))
  })

  it('stays disabled while the loaded view has nothing unread', async () => {
    inbox = [message(1, 'First mail', true)]
    await mount()

    expect(screen.getByRole('button', { name: '全部已读' })).toBeDisabled()
    expect(calls.some(call => call.includes('mark-read'))).toBe(false)
  })
})
