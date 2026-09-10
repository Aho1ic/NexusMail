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
  // null means "let the fake derive it from the page", which is what a view smaller
  // than one page looks like.
  let unreadTotal: number | null
  // Makes the flag write fail the way a provider that cannot select the message's
  // folder does. The browser fires this PATCH without awaiting it, so what the UI
  // does with the failure is the whole contract.
  let patchFails: boolean

  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    inbox = [message(1, 'First mail', false), message(2, 'Second mail', false)]
    markRead = { updated: 2 }
    calls = []
    unreadTotal = null
    patchFails = false
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
      const detail = url.match(/^\/api\/v1\/messages\/(\d+)$/)
      if (detail) {
        const item = inbox.find(entry => entry.id === Number(detail[1]))
        if ((init?.method ?? 'GET').toUpperCase() === 'PATCH') {
          if (patchFails) return json({ error: { code: 'internal_error', message: '文件夹不存在' } }, 500)
          return json(item)
        }
        return json({ message: item, attachments: [] })
      }
      if (url.startsWith('/api/v1/messages')) {
        const total = unreadTotal ?? inbox.filter(item => !item.is_read).length
        return json({ items: inbox, unread_total: total })
      }
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
    // The sidebar count comes from the server total that ships with the feed.
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

  it('counts the whole view, not the loaded page', async () => {
    // One page holds 40 rows while the view holds far more unread mail. Counting the
    // loaded rows reported 2 here, and reported 0 for any view whose unread mail sat
    // entirely past the first page, which left the button disabled with work to do.
    unreadTotal = 137
    await mount()

    expect(screen.getByText('137')).toBeInTheDocument()
    const button = screen.getByRole('button', { name: '全部已读' })
    expect(button).toBeEnabled()
    expect(button).toHaveAttribute('title', '将当前视图的 137 封未读邮件标记为已读')
  })

  it('offers the pass when every unread message is past the first page', async () => {
    // Every loaded row is read, yet the view still holds unread mail further down.
    inbox = [message(1, 'First mail', true), message(2, 'Second mail', true)]
    unreadTotal = 12
    await mount()

    expect(screen.getByRole('button', { name: '全部已读' })).toBeEnabled()
    expect(screen.getByText('12')).toBeInTheDocument()
  })

  it('draws the badge down when a message is opened', async () => {
    unreadTotal = 9
    await mount()
    expect(screen.getByText('9')).toBeInTheDocument()

    // The badge tracks the server total, so opening a message has to adjust it locally
    // as well; otherwise the count only moves on the next feed load.
    unreadTotal = 8
    fireEvent.click(screen.getByRole('button', { name: /First mail/ }))

    await waitFor(() => expect(screen.getByText('8')).toBeInTheDocument())
    await waitFor(() => expect(calls).toContain('PATCH /api/v1/messages/1'))
  })

  // The regression for the reported defect. Opening a message draws it read before
  // the server has agreed, and the failure used to be swallowed outright, so the UI
  // kept claiming read while the message was still unread everywhere else — the user
  // only found out on the next feed load, which is what "mail I read goes back to
  // unread after refreshing" is. Nothing reloads the feed on this path, so the badge
  // returning to its old value can only come from the rollback.
  it('puts the message back to unread when the flag write fails', async () => {
    unreadTotal = 9
    patchFails = true
    await mount()
    expect(screen.getByText('9')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /First mail/ }))

    await waitFor(() => expect(screen.getByText('8')).toBeInTheDocument())
    await waitFor(() => expect(screen.getByText('9')).toBeInTheDocument())
    expect(await screen.findByRole('status')).toHaveTextContent('标记已读失败：文件夹不存在')
  })

  it('returns to login when marking a message read rejects the session', async () => {
    await mount()
    const originalFetch = fetch
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'PATCH') {
        return Promise.resolve(json({ error: { code: 'unauthorized', message: 'invalid session or CSRF token' } }, 401))
      }
      return originalFetch(input, init)
    }))

    fireEvent.click(screen.getByRole('button', { name: /First mail/ }))

    expect(await screen.findByLabelText('API Key')).toBeInTheDocument()
    expect(sessionStorage.getItem('nexusmail.csrf')).toBeNull()
    // The lapsed session must not be reported as a mark-read failure: the rollback
    // happens, the login screen arrives, and the server's English string never
    // reaches the toast.
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    cleanup()
    render(<App />)
    expect(screen.getByLabelText('API Key')).toBeInTheDocument()
  })

  // The reported defect: "标记已读失败：authentication required". A cookieless request is
  // what the server answers with that English string, and the cookie lapses on its
  // own - it carries a fixed Expires and is never renewed - while the CSRF token in
  // localStorage stays readable.
  //
  // Normally the 401 clears the session and the login screen replaces the mailbox
  // before any toast can render. Not here: `request()` only clears when the token it
  // captured is still current, so a token that changed while the unawaited PATCH was
  // in flight - another tab signing in - makes it skip the clear and reject into a
  // mailbox that is still on screen. That is the path that reached the toast.
  it('does not blame mark-read for a lapsed session that stays mounted', async () => {
    unreadTotal = 9
    localStorage.setItem('nexusmail.csrf', 'first-csrf')
    await mount()
    expect(screen.getByText('9')).toBeInTheDocument()
    const originalFetch = fetch
    let patched = false
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (/^\/api\/v1\/messages\/\d+$/.test(url) && (init?.method ?? 'GET').toUpperCase() === 'PATCH') {
        patched = true
        // Another tab logs in while this write is in flight, so the token it was
        // captured against is no longer current and the session survives the 401.
        localStorage.setItem('nexusmail.csrf', 'second-csrf')
        return json({ error: { code: 'unauthorized', message: 'authentication required' } }, 401)
      }
      return originalFetch(input, init)
    }))

    fireEvent.click(screen.getByRole('button', { name: /First mail/ }))

    await waitFor(() => expect(patched).toBe(true))
    // The row still rolls back - the badge returns to its old value - and the mailbox
    // is still mounted, which is exactly why the toast could appear.
    await waitFor(() => expect(screen.getByText('9')).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /First mail/ })).toBeInTheDocument()
    expect(screen.queryByText(/标记已读失败/)).not.toBeInTheDocument()
    expect(screen.queryByText(/authentication required/)).not.toBeInTheDocument()
  })
})
