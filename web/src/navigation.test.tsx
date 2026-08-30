import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// The shell decides which mail the user is looking at, and every request it makes
// has to describe exactly that view. A scope that drifts from the list — a stale
// account_id, a dropped search term, a cursor from another folder — shows one set of
// mail while acting on another, which is how mail goes missing.

class FakeSocket {
  static instances: FakeSocket[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  closed = 0
  constructor(public url: string) { FakeSocket.instances.push(this) }
  close() { this.closed += 1 }
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const accounts = [
  { id: 1, email: 'work@qq.com', display_name: '工作', provider: 'qq', status: 'connected' },
  { id: 2, email: 'alt@163.com', display_name: '', provider: '163', status: 'backoff' },
]

const mailboxes = [
  { id: 11, account_id: 1, remote_name: 'INBOX', display_name: '收件箱', role: 'inbox', sync_mode: 'realtime' },
  { id: 12, account_id: 1, remote_name: 'Sent', display_name: '已发送', role: 'sent', sync_mode: 'periodic' },
]

function message(id: number, subject: string, extra: Record<string, unknown> = {}) {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'work@qq.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'preview',
    body_state: 'ready', body_text: 'body', received_at: 1767000000000,
    is_read: true, is_starred: false, has_attachments: false, ...extra,
  }
}

type Recorder = {
  feeds: URLSearchParams[]
  writes: Array<{ method: string; url: string; body: unknown }>
  all: string[]
}

type Options = {
  page?: (params: URLSearchParams, index: number) => unknown
  accountsReply?: () => Response
  detail?: (id: number) => Response
  patch?: (id: number, body: unknown) => Response
}

function stubAPI(options: Options = {}) {
  const recorder: Recorder = { feeds: [], writes: [], all: [] }
  let feedIndex = 0
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    recorder.all.push(`${method} ${url}`)
    if (method !== 'GET') recorder.writes.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : null })

    if (url === '/api/v1/accounts') return options.accountsReply?.() ?? json({ items: accounts })
    const folders = url.match(/^\/api\/v1\/accounts\/(\d+)\/mailboxes$/)
    // Folders are per account, so account 2 has none here: a stub that answered the
    // same list for every account could not tell a scoped view from an unscoped one.
    if (folders) return json({ items: mailboxes.filter(box => box.account_id === Number(folders[1])) })
    const detail = url.match(/^\/api\/v1\/messages\/(\d+)$/)
    if (detail && method === 'GET') {
      return options.detail?.(Number(detail[1])) ?? json({ message: message(Number(detail[1]), `主题 ${detail[1]}`), attachments: [] })
    }
    if (detail && method === 'PATCH') {
      const body = init?.body ? JSON.parse(String(init.body)) : {}
      return options.patch?.(Number(detail[1]), body) ?? json(message(Number(detail[1]), `主题 ${detail[1]}`, body))
    }
    if (url.startsWith('/api/v1/messages?')) {
      const params = new URLSearchParams(url.slice(url.indexOf('?') + 1))
      recorder.feeds.push(params)
      const reply = options.page?.(params, feedIndex)
      feedIndex += 1
      return json(reply ?? { items: [message(1, '第一封'), message(2, '第二封')], unread_total: 0 })
    }
    if (url === '/api/v1/drafts') return json({ items: [] })
    return json({})
  }))
  return recorder
}

const lastFeed = (recorder: Recorder) => recorder.feeds[recorder.feeds.length - 1]

// Each message row also carries its account name as a chip, so the sidebar entry is
// addressed by the address it shows as a sublabel, which appears nowhere else.
const navAccount = (email: string) => screen.getByRole('button', { name: new RegExp(email.replace('.', '\\.')) })

describe('view scoping', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  async function boot(options: Options = {}) {
    const recorder = stubAPI(options)
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    return recorder
  }

  it('opens on every inbox and names the view', async () => {
    const recorder = await boot()
    expect(screen.getByRole('heading', { name: 'All Inboxes' })).toBeInTheDocument()
    // No account chosen: the server is asked for the inbox role across accounts.
    expect(lastFeed(recorder).get('folder')).toBe('inbox')
    expect(lastFeed(recorder).get('account_id')).toBeNull()
    expect(lastFeed(recorder).get('limit')).toBe('40')
  })

  it('scopes to one account and titles the list with it', async () => {
    const recorder = await boot()
    fireEvent.click(navAccount('work@qq.com'))

    await waitFor(() => expect(lastFeed(recorder).get('account_id')).toBe('1'))
    expect(lastFeed(recorder).get('folder')).toBe('inbox')
    expect(screen.getByRole('heading', { name: '工作' })).toBeInTheDocument()
    // The account's mailboxes are fetched so the folder list can expand.
    expect(recorder.all).toContain('GET /api/v1/accounts/1/mailboxes')
  })

  it('falls back to the address when an account has no display name', async () => {
    await boot()
    expect(navAccount('alt@163.com')).toBeInTheDocument()
  })

  it('scopes to a mailbox and drops the folder role', async () => {
    const recorder = await boot()
    fireEvent.click(navAccount('work@qq.com'))
    fireEvent.click(await screen.findByRole('button', { name: '已发送' }))

    await waitFor(() => expect(lastFeed(recorder).get('mailbox_id')).toBe('12'))
    // mailbox_id is exact, so folder=inbox would contradict it.
    expect(lastFeed(recorder).get('folder')).toBeNull()
    expect(screen.getByRole('heading', { name: '已发送' })).toBeInTheDocument()
  })

  it('returns to every inbox and clears both scopes', async () => {
    const recorder = await boot()
    fireEvent.click(navAccount('work@qq.com'))
    // Go all the way down to a folder first: returning from there is what has to
    // release both scopes, and a lingering mailbox_id would show one folder of one
    // account under a heading that claims to show everything.
    fireEvent.click(await screen.findByRole('button', { name: '已发送' }))
    await waitFor(() => expect(lastFeed(recorder).get('mailbox_id')).toBe('12'))

    fireEvent.click(screen.getByRole('button', { name: /All Inboxes/ }))
    await waitFor(() => expect(lastFeed(recorder).get('account_id')).toBeNull())
    expect(lastFeed(recorder).get('mailbox_id')).toBeNull()
    expect(lastFeed(recorder).get('folder')).toBe('inbox')
    expect(screen.getByRole('heading', { name: 'All Inboxes' })).toBeInTheDocument()
  })

  it('releases the mailbox scope when the account changes', async () => {
    const recorder = await boot()
    fireEvent.click(navAccount('work@qq.com'))
    fireEvent.click(await screen.findByRole('button', { name: '已发送' }))
    await waitFor(() => expect(lastFeed(recorder).get('mailbox_id')).toBe('12'))

    // Mailbox 12 belongs to account 1; carrying it to account 2 would ask for a
    // folder that account cannot see.
    fireEvent.click(navAccount('alt@163.com'))
    await waitFor(() => expect(lastFeed(recorder).get('account_id')).toBe('2'))
    expect(lastFeed(recorder).get('mailbox_id')).toBeNull()
    expect(lastFeed(recorder).get('folder')).toBe('inbox')
  })

  it('collapses back to the account view when its row is pressed again', async () => {
    const recorder = await boot()
    fireEvent.click(navAccount('work@qq.com'))
    fireEvent.click(await screen.findByRole('button', { name: '已发送' }))
    await waitFor(() => expect(lastFeed(recorder).get('mailbox_id')).toBe('12'))

    // The account does not change here, so nothing but the handler can reset the
    // mailbox — and without it the folder title would contradict the feed.
    fireEvent.click(navAccount('work@qq.com'))
    await waitFor(() => expect(lastFeed(recorder).get('mailbox_id')).toBeNull())
    expect(lastFeed(recorder).get('account_id')).toBe('1')
    expect(screen.getByRole('heading', { name: '工作' })).toBeInTheDocument()
  })

  it('shows only the selected account’s folders', async () => {
    await boot()
    expect(screen.queryByRole('button', { name: '收件箱' })).not.toBeInTheDocument()
    fireEvent.click(navAccount('work@qq.com'))
    expect(await screen.findByRole('button', { name: '收件箱' })).toBeInTheDocument()

    fireEvent.click(navAccount('alt@163.com'))
    await waitFor(() => expect(screen.queryByRole('button', { name: '收件箱' })).not.toBeInTheDocument())
  })
})

describe('search', () => {
  afterEach(() => { cleanup(); vi.useRealTimers() })
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  async function boot(options: Options = {}) {
    const recorder = stubAPI(options)
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    return recorder
  }

  const searchBox = () => screen.getByPlaceholderText('搜索主题、发件人或正文…')

  it('debounces typing into a single query and trims it', async () => {
    const recorder = await boot()
    const before = recorder.feeds.length

    for (const value of ['发', '发票', '发票 ']) fireEvent.change(searchBox(), { target: { value } })
    // Nothing yet: a request per keystroke would be three searches.
    expect(recorder.feeds.length).toBe(before)

    await vi.advanceTimersByTimeAsync(300)
    await waitFor(() => expect(recorder.feeds.length).toBe(before + 1))
    expect(lastFeed(recorder).get('query')).toBe('发票')
  })

  it('keeps the account scope while searching', async () => {
    const recorder = await boot()
    fireEvent.click(navAccount('work@qq.com'))
    await waitFor(() => expect(lastFeed(recorder).get('account_id')).toBe('1'))

    fireEvent.change(searchBox(), { target: { value: '发票' } })
    await vi.advanceTimersByTimeAsync(300)
    await waitFor(() => expect(lastFeed(recorder).get('query')).toBe('发票'))
    expect(lastFeed(recorder).get('account_id')).toBe('1')
  })

  it('clears the term and the filter with the clear control', async () => {
    const recorder = await boot()
    fireEvent.change(searchBox(), { target: { value: '发票' } })
    await vi.advanceTimersByTimeAsync(300)
    await waitFor(() => expect(lastFeed(recorder).get('query')).toBe('发票'))

    // The clear button only exists while a term is present.
    const clear = within(searchBox().parentElement as HTMLElement).getByRole('button')
    fireEvent.click(clear)
    await vi.advanceTimersByTimeAsync(300)
    await waitFor(() => expect(lastFeed(recorder).get('query')).toBeNull())
    expect(searchBox()).toHaveValue('')
  })

  it('does not search on whitespace alone', async () => {
    const recorder = await boot()
    fireEvent.change(searchBox(), { target: { value: '   ' } })
    await vi.advanceTimersByTimeAsync(300)
    // A blank term is not a filter, so the unfiltered view stands.
    expect(lastFeed(recorder).get('query')).toBeNull()
  })

  it('reports an empty result rather than an empty screen', async () => {
    const recorder = await boot({ page: params => params.get('query') ? { items: [], unread_total: 0 } : undefined })
    fireEvent.change(searchBox(), { target: { value: '没有这封' } })
    await vi.advanceTimersByTimeAsync(300)
    expect(await screen.findByText('这里空空如也')).toBeInTheDocument()
    expect(lastFeed(recorder).get('query')).toBe('没有这封')
  })
})

describe('pagination', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  it('offers more only while the server hands back a cursor', async () => {
    stubAPI({ page: () => ({ items: [message(1, '第一封')], unread_total: 0 }) })
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    expect(screen.queryByRole('button', { name: '加载更多' })).not.toBeInTheDocument()
  })

  it('appends the next page with the cursor the server gave', async () => {
    const recorder = stubAPI({
      page: (params, index) => index === 0
        ? { items: [message(1, '第一封')], next_cursor: 'cursor-1', unread_total: 0 }
        : { items: [message(2, '第二封')], unread_total: 0 },
    })
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: '加载更多' }))

    // Appended, not replaced: the first page stays on screen.
    expect(await screen.findByRole('button', { name: /第二封/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /第一封/ })).toBeInTheDocument()
    expect(lastFeed(recorder).get('cursor')).toBe('cursor-1')
    // Exhausted: no cursor came back, so the control retires.
    await waitFor(() => expect(screen.queryByRole('button', { name: '加载更多' })).not.toBeInTheDocument())
  })

  it('does not duplicate a row the next page repeats', async () => {
    stubAPI({
      page: (params, index) => index === 0
        ? { items: [message(1, '第一封'), message(2, '第二封')], next_cursor: 'cursor-1', unread_total: 0 }
        // A message that arrived mid-page shifts the window, so the boundary row
        // comes back a second time.
        : { items: [message(2, '第二封'), message(3, '第三封')], unread_total: 0 },
    })
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: '加载更多' }))
    await screen.findByRole('button', { name: /第三封/ })
    expect(screen.getAllByRole('button', { name: /第二封/ })).toHaveLength(1)
  })

  it('drops the cursor when the view changes', async () => {
    const recorder = stubAPI({
      page: params => params.get('account_id')
        ? { items: [message(3, '第三封')], unread_total: 0 }
        : { items: [message(1, '第一封')], next_cursor: 'cursor-1', unread_total: 0 },
    })
    render(<App />)
    await screen.findByRole('button', { name: '加载更多' })

    fireEvent.click(navAccount('work@qq.com'))
    await screen.findByRole('button', { name: /第三封/ })
    // A cursor from the unscoped feed means nothing in the scoped one.
    expect(lastFeed(recorder).get('cursor')).toBeNull()
    expect(screen.queryByRole('button', { name: /第一封/ })).not.toBeInTheDocument()
  })
})

describe('message actions', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  async function openFirst(options: Options = {}) {
    const recorder = stubAPI(options)
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /第一封/ }))
    await screen.findByRole('button', { name: /回复/ })
    return recorder
  }

  it('stars through the server and reflects what came back', async () => {
    const recorder = await openFirst()
    fireEvent.click(screen.getByRole('button', { name: '星标' }))

    await waitFor(() => expect(recorder.writes.some(write => write.url === '/api/v1/messages/1' && (write.body as { is_starred?: boolean }).is_starred === true)).toBe(true))
  })

  it('unstars a starred message rather than starring it again', async () => {
    const recorder = await openFirst({
      page: () => ({ items: [message(1, '第一封', { is_starred: true })], unread_total: 0 }),
      detail: id => json({ message: message(id, '第一封', { is_starred: true }), attachments: [] }),
    })
    fireEvent.click(screen.getByRole('button', { name: '星标' }))
    await waitFor(() => expect(recorder.writes.at(-1)?.body).toEqual({ is_starred: false }))
  })

  it('archives, removes the row and clears the reading pane', async () => {
    const recorder = await openFirst()
    fireEvent.click(screen.getByRole('button', { name: '归档 (e)' }))

    await waitFor(() => expect(screen.queryByRole('button', { name: /第一封/ })).not.toBeInTheDocument())
    expect(recorder.writes.at(-1)).toMatchObject({ method: 'PATCH', url: '/api/v1/messages/1', body: { archive: true } })
    // The pane cannot keep showing mail that is no longer in the view.
    expect(screen.getByText('收件箱已就绪')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /第二封/ })).toBeInTheDocument()
  })

  it('surfaces a rejected action and keeps the message on screen', async () => {
    await openFirst({ patch: () => json({ error: { code: 'internal', message: '归档失败' } }, 500) })
    fireEvent.click(screen.getByRole('button', { name: '归档 (e)' }))

    expect(await screen.findByText('归档失败')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /第一封/ })).toBeInTheDocument()
  })

  it('surfaces a failed detail load without losing the list', async () => {
    await openFirst({ detail: () => json({ error: { code: 'not_found', message: '邮件不存在' } }, 404) })
    expect(await screen.findByText('邮件不存在')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /第二封/ })).toBeInTheDocument()
  })

  it('opens the composer as a reply to the message being read', async () => {
    await openFirst()
    fireEvent.click(screen.getByRole('button', { name: /回复/ }))
    expect(await screen.findByLabelText('收件人')).toHaveValue('sender@example.com')
    // The reply is built from the feed row, which is what the shell holds as the
    // selection; the detail payload only fills the reading pane.
    expect(screen.getByLabelText('主题')).toHaveValue('Re: 第一封')
  })
})

describe('session and refresh', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  it('reloads accounts, folders and mail on refresh', async () => {
    const recorder = stubAPI()
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /work@qq\.com/ }))
    await waitFor(() => expect(lastFeed(recorder).get('account_id')).toBe('1'))
    const before = recorder.all.length

    fireEvent.click(screen.getByRole('button', { name: '刷新' }))
    await waitFor(() => expect(recorder.all.length).toBeGreaterThan(before))
    const after = recorder.all.slice(before)
    expect(after).toContain('GET /api/v1/accounts')
    expect(after).toContain('GET /api/v1/accounts/1/mailboxes')
    expect(after.some(entry => entry.startsWith('GET /api/v1/messages?'))).toBe(true)
  })

  it('ends the session on the server and returns to the key screen', async () => {
    const recorder = stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })

    fireEvent.click(screen.getByRole('button', { name: /退出/ }))
    expect(await screen.findByLabelText('API Key')).toBeInTheDocument()
    expect(recorder.writes.at(-1)).toMatchObject({ method: 'DELETE', url: '/api/v1/auth/session' })
    // The stored token has to go, or a reload would land in a dead mailbox.
    expect(sessionStorage.getItem('nexusmail.csrf')).toBeNull()
  })

  it('returns to the key screen even when the logout call fails', async () => {
    stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    // Locally the session is over regardless of what the server says.
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))

    fireEvent.click(screen.getByRole('button', { name: /退出/ }))
    expect(await screen.findByLabelText('API Key')).toBeInTheDocument()
  })

  it('falls back to the key screen when the session has expired server-side', async () => {
    stubAPI({ accountsReply: () => json({ error: { code: 'unauthorized', message: 'authentication required' } }, 401) })
    render(<App />)
    // A 401 on the accounts probe means the cookie is gone; showing an error
    // banner over an empty mailbox would leave no way back in.
    expect(await screen.findByLabelText('API Key')).toBeInTheDocument()
  })

  it('reports a non-401 account failure without ending the session', async () => {
    stubAPI({ accountsReply: () => json({ error: { code: 'internal', message: '账户列表读取失败' } }, 500) })
    render(<App />)
    expect(await screen.findByText('账户列表读取失败')).toBeInTheDocument()
    expect(screen.queryByLabelText('API Key')).not.toBeInTheDocument()
  })

  it('reports a failed feed load', async () => {
    stubAPI({ page: () => { throw new Error('unused') } })
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/accounts') return json({ items: accounts })
      if (url.includes('/mailboxes')) return json({ items: mailboxes })
      if (url.startsWith('/api/v1/messages')) return json({ error: { code: 'internal', message: '邮件列表读取失败' } }, 500)
      return json({})
    }))
    render(<App />)
    expect(await screen.findByText('邮件列表读取失败')).toBeInTheDocument()
  })

  it('reports a failed folder load', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/accounts') return json({ items: accounts })
      if (url.includes('/mailboxes')) return json({ error: { code: 'internal', message: '文件夹读取失败' } }, 500)
      if (url.startsWith('/api/v1/messages')) return json({ items: [message(1, '第一封')], unread_total: 0 })
      return json({})
    }))
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /work@qq\.com/ }))
    expect(await screen.findByText('文件夹读取失败')).toBeInTheDocument()
  })

  it('keeps one socket across view changes and closes it on logout', async () => {
    const recorder = stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    expect(FakeSocket.instances).toHaveLength(1)

    fireEvent.click(navAccount('work@qq.com'))
    await waitFor(() => expect(lastFeed(recorder).get('account_id')).toBe('1'))
    expect(FakeSocket.instances).toHaveLength(1)

    fireEvent.click(screen.getByRole('button', { name: /退出/ }))
    await screen.findByLabelText('API Key')
    expect(FakeSocket.instances[0].closed).toBeGreaterThan(0)
  })
})

describe('dialogs and panes', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  async function boot() {
    const recorder = stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    return recorder
  }

  it('opens the outbox and hands a draft to the composer for editing', async () => {
    const editable = {
      id: 5, account_id: 1, revision: 2, to: JSON.stringify(['her@example.com']), cc: '[]', bcc: '[]',
      subject: '待续', body_text: '草稿正文', status: 'failed', remote_sync_state: 'synced', updated_at: 0,
    }
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/accounts') return json({ items: accounts })
      if (url.includes('/mailboxes')) return json({ items: mailboxes })
      if (url === '/api/v1/drafts') return json({ items: [editable] })
      if (url.startsWith('/api/v1/messages')) return json({ items: [message(1, '第一封')], unread_total: 0 })
      return json({})
    }))
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /草稿与发件箱/ }))

    fireEvent.click(await screen.findByText('编辑'))
    // The outbox closes and the composer opens on that draft, not a blank one.
    expect(await screen.findByLabelText('收件人')).toHaveValue('her@example.com')
    expect(screen.getByLabelText('主题')).toHaveValue('待续')
    expect(screen.queryByText('草稿与发件箱', { selector: 'h2' })).not.toBeInTheDocument()
  })

  it('opens the connect dialog from the sidebar and from settings', async () => {
    await boot()
    fireEvent.click(screen.getByRole('button', { name: /连接邮箱/ }))
    expect(await screen.findByRole('heading', { name: '连接邮箱' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    await waitFor(() => expect(screen.queryByRole('heading', { name: '连接邮箱' })).not.toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: '设置' }))
    fireEvent.click(await within(await screen.findByRole('dialog', { name: '设置' })).findByRole('button', { name: /连接邮箱/ }))
    // Settings gets out of the way rather than stacking two dialogs.
    expect(await screen.findByRole('heading', { name: '连接邮箱' })).toBeInTheDocument()
    expect(screen.queryByRole('dialog', { name: '设置' })).not.toBeInTheDocument()
  })

  it('reloads the account list after one is connected', async () => {
    const recorder = await boot()
    fireEvent.click(screen.getByRole('button', { name: /连接邮箱/ }))
    await screen.findByRole('heading', { name: '连接邮箱' })

    fireEvent.change(screen.getByLabelText('显示名称'), { target: { value: '新号' } })
    fireEvent.change(screen.getByLabelText('邮箱地址'), { target: { value: 'new@qq.com' } })
    fireEvent.change(screen.getByLabelText('授权码'), { target: { value: 'code' } })
    const before = recorder.all.filter(entry => entry === 'GET /api/v1/accounts').length
    fireEvent.submit(screen.getByRole('button', { name: '继续' }).closest('form') as HTMLFormElement)

    await waitFor(() => expect(recorder.all.filter(entry => entry === 'GET /api/v1/accounts').length).toBeGreaterThan(before))
    expect(screen.queryByRole('heading', { name: '连接邮箱' })).not.toBeInTheDocument()
  })

  it('refreshes the feed after a message is sent and closes the composer', async () => {
    const recorder = await boot()
    fireEvent.click(screen.getByRole('button', { name: '写邮件' }))
    fireEvent.change(await screen.findByLabelText('收件人'), { target: { value: 'a@example.com' } })
    const before = recorder.all.length
    fireEvent.click(screen.getByRole('button', { name: /发送/ }))

    await waitFor(() => expect(screen.queryByLabelText('收件人')).not.toBeInTheDocument())
    // The sent copy shows up in the feed only if it is reloaded.
    expect(recorder.all.slice(before).some(entry => entry.startsWith('GET /api/v1/messages?'))).toBe(true)
  })

  it('lets the composer be abandoned without sending', async () => {
    const recorder = await boot()
    fireEvent.click(screen.getByRole('button', { name: '写邮件' }))
    await screen.findByLabelText('收件人')
    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    await waitFor(() => expect(screen.queryByLabelText('收件人')).not.toBeInTheDocument())
    expect(recorder.writes).toHaveLength(0)
  })

  // Below the md breakpoint the three panes are shown one at a time, and the only
  // way between them is these two buttons. Which pane is current is carried in a
  // class, so it is asserted there rather than through visibility.
  it('moves between the folder list, the feed and the reading pane on a narrow screen', async () => {
    await boot()
    const aside = screen.getByRole('button', { name: /All Inboxes/ }).closest('aside') as HTMLElement
    const feed = screen.getByLabelText('邮件列表').closest('section') as HTMLElement
    expect(feed.className).toContain('flex')
    expect(aside.className).toContain('hidden')

    fireEvent.click(screen.getByRole('button', { name: '打开文件夹' }))
    await waitFor(() => expect(aside.className).toContain('flex'))
    expect(feed.className).toContain('hidden')

    // Choosing a folder hands the screen back to the feed.
    fireEvent.click(screen.getByRole('button', { name: /All Inboxes/ }))
    await waitFor(() => expect(feed.className).toContain('flex'))

    // Opening mail hands it to the reading pane, and the back button returns.
    fireEvent.click(screen.getByRole('button', { name: /第一封/ }))
    await screen.findByRole('button', { name: /回复/ })
    expect(feed.className).toContain('hidden')
    fireEvent.click(screen.getByRole('button', { name: '返回列表' }))
    await waitFor(() => expect(feed.className).toContain('flex'))
  })
})
