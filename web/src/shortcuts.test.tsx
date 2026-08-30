import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { useKeyboard } from './hooks/useKeyboard'
import { notificationsEnabled, notify } from './hooks/useRealtime'
import { prepareMessageHTML } from './lib/messagehtml'
import { copyText } from './lib/notifications'
import { decodeEncodedWords, displaySender, formatBytes, formatDate, formatFullDate, messageOf, splitEmails } from './lib/format'
import type { Message } from './types'

// settings.test.tsx pins that the shortcut setting is honoured. This covers the
// keys themselves — where each one lands at the ends of the list, and the rule that
// they never fire while the user is typing, which would archive mail mid-sentence.

class FakeSocket {
  static instances: FakeSocket[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  constructor(public url: string) { FakeSocket.instances.push(this) }
  close() { this.onclose?.() }
  emit(type: string) { this.onmessage?.({ data: JSON.stringify({ type, sequence: 1, occurred_at: 0, data: {} }) }) }
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'work@qq.com', display_name: '工作', provider: 'qq', status: 'connected' }

function message(id: number, subject: string): Message {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'work@qq.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'preview',
    body_state: 'ready', body_text: 'body', received_at: 1767000000000,
    is_read: true, is_starred: false, has_attachments: false,
  }
}

const inbox = [message(1, '第一封'), message(2, '第二封'), message(3, '第三封')]

function stubAPI() {
  const writes: Array<{ method: string; url: string; body: unknown }> = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    if (method !== 'GET') writes.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : null })
    if (url === '/api/v1/accounts') return json({ items: [account] })
    if (url.includes('/mailboxes')) return json({ items: [] })
    const detail = url.match(/^\/api\/v1\/messages\/(\d+)$/)
    if (detail && method === 'GET') return json({ message: inbox.find(item => item.id === Number(detail[1])), attachments: [] })
    if (detail) return json(message(Number(detail[1]), '第一封'))
    if (url.startsWith('/api/v1/messages')) return json({ items: inbox, unread_total: 0 })
    return json({})
  }))
  return writes
}

describe('keyboard shortcuts', () => {
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
    const writes = stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    // findByRole resolves on the DOM mutation at commit time, which can land before
    // the passive effect that re-registers the key handler over the now-populated
    // message list. A key fired in that window is handled by the previous closure,
    // finds an empty list, and is dropped — and nothing re-fires it, so the waitFor
    // that follows can never pass. This flush closes that window. It only opens
    // under load, which is why the failure was rare and appeared under coverage.
    await act(async () => undefined)
    return writes
  }

  // Both the feed and the reading pane carry an h1, so the subject is read from the
  // article the detail view renders into.
  const reading = () => document.querySelector('article h1')?.textContent

  it('walks down the list with j and stops at the end', async () => {
    await boot()
    fireEvent.keyDown(window, { key: 'j' })
    await waitFor(() => expect(reading()).toBe('第一封'))
    fireEvent.keyDown(window, { key: 'j' })
    await waitFor(() => expect(reading()).toBe('第二封'))
    fireEvent.keyDown(window, { key: 'j' })
    await waitFor(() => expect(reading()).toBe('第三封'))
    // Past the last row there is nothing to open, so the selection holds.
    fireEvent.keyDown(window, { key: 'j' })
    await waitFor(() => expect(reading()).toBe('第三封'))
  })

  it('walks back up with k and stops at the top', async () => {
    await boot()
    fireEvent.click(screen.getByRole('button', { name: /第三封/ }))
    await waitFor(() => expect(reading()).toBe('第三封'))

    fireEvent.keyDown(window, { key: 'k' })
    await waitFor(() => expect(reading()).toBe('第二封'))
    fireEvent.keyDown(window, { key: 'k' })
    await waitFor(() => expect(reading()).toBe('第一封'))
    fireEvent.keyDown(window, { key: 'k' })
    await waitFor(() => expect(reading()).toBe('第一封'))
  })

  it('opens the first message with k when nothing is selected', async () => {
    await boot()
    // index is -1, and Math.max(0, -2) lands on the first row rather than throwing.
    fireEvent.keyDown(window, { key: 'k' })
    await waitFor(() => expect(reading()).toBe('第一封'))
  })

  it('opens the composer with c and archives with e', async () => {
    const writes = await boot()
    fireEvent.keyDown(window, { key: 'c' })
    expect(await screen.findByLabelText('收件人')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    await waitFor(() => expect(screen.queryByLabelText('收件人')).not.toBeInTheDocument())

    // e needs a selection: there is nothing to archive otherwise.
    fireEvent.keyDown(window, { key: 'e' })
    expect(writes.filter(write => write.method === 'PATCH')).toHaveLength(0)

    fireEvent.click(screen.getByRole('button', { name: /第二封/ }))
    await waitFor(() => expect(reading()).toBe('第二封'))
    fireEvent.keyDown(window, { key: 'e' })
    await waitFor(() => expect(writes.some(write => write.url === '/api/v1/messages/2' && (write.body as { archive?: boolean }).archive)).toBe(true))
  })

  it('ignores an unbound key', async () => {
    await boot()
    fireEvent.keyDown(window, { key: 'x' })
    // Nothing opened, so the welcome pane is still showing.
    expect(screen.getByText('收件箱已就绪')).toBeInTheDocument()
  })

  it('never fires while the user is typing', async () => {
    await boot()
    const search = screen.getByPlaceholderText('搜索主题、发件人或正文…')
    for (const key of ['j', 'k', 'c', 'e']) fireEvent.keyDown(search, { key })
    expect(screen.getByText('收件箱已就绪')).toBeInTheDocument()
    expect(screen.queryByLabelText('收件人')).not.toBeInTheDocument()

    // Same for a textarea and a select, which is where a body and an account are
    // chosen — 'e' there would archive the mail being replied to.
    fireEvent.click(screen.getByRole('button', { name: '写邮件' }))
    const body = await screen.findByPlaceholderText('写点什么…')
    fireEvent.keyDown(body, { key: 'e' })
    fireEvent.keyDown(screen.getByRole('combobox'), { key: 'e' })
    expect(screen.getByPlaceholderText('写点什么…')).toBeInTheDocument()
  })

  it('stops listening once the shortcut setting is turned off', async () => {
    await boot()
    fireEvent.click(screen.getByRole('button', { name: '设置' }))
    fireEvent.click(await screen.findByRole('switch', { name: '启用单键快捷键' }))
    fireEvent.click(screen.getByRole('button', { name: '完成' }))

    fireEvent.keyDown(window, { key: 'j' })
    expect(screen.getByText('收件箱已就绪')).toBeInTheDocument()
  })
})

// The hook takes its actions as opaque callbacks, so it cannot rely on the caller
// guarding them. Driving it directly is the only place its own preconditions are
// visible: through App, mutateMessage's `if (!selected) return` would mask them.
describe('the keyboard hook in isolation', () => {
  afterEach(cleanup)

  function probe(enabled: boolean, selected: ReturnType<typeof message> | null, list = inbox) {
    const open = vi.fn()
    const compose = vi.fn()
    const archive = vi.fn()
    function Probe() {
      useKeyboard(enabled, list, selected, open, compose, archive)
      return null
    }
    render(<Probe />)
    return { open, compose, archive }
  }

  it('does not archive when nothing is selected', () => {
    const { archive } = probe(true, null)
    fireEvent.keyDown(window, { key: 'e' })
    expect(archive).not.toHaveBeenCalled()
  })

  it('archives the selected message', () => {
    const { archive } = probe(true, inbox[1])
    fireEvent.keyDown(window, { key: 'e' })
    expect(archive).toHaveBeenCalledTimes(1)
  })

  it('composes whether or not anything is selected', () => {
    const { compose } = probe(true, null)
    fireEvent.keyDown(window, { key: 'c' })
    expect(compose).toHaveBeenCalledTimes(1)
  })

  it('binds nothing while disabled', () => {
    const { open, compose, archive } = probe(false, inbox[0])
    for (const key of ['j', 'k', 'c', 'e']) fireEvent.keyDown(window, { key })
    expect(open).not.toHaveBeenCalled()
    expect(compose).not.toHaveBeenCalled()
    expect(archive).not.toHaveBeenCalled()
  })

  it('does nothing on an empty list', () => {
    const { open } = probe(true, null, [])
    fireEvent.keyDown(window, { key: 'j' })
    fireEvent.keyDown(window, { key: 'k' })
    expect(open).not.toHaveBeenCalled()
  })

  it('unbinds on unmount', () => {
    const archive = vi.fn()
    function Probe() {
      useKeyboard(true, inbox, inbox[0], () => undefined, () => undefined, archive)
      return null
    }
    render(<Probe />).unmount()
    fireEvent.keyDown(window, { key: 'e' })
    expect(archive).not.toHaveBeenCalled()
  })
})

describe('realtime reconnection', () => {
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

  it('reconnects after a drop and resyncs on open', async () => {
    stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    expect(FakeSocket.instances).toHaveLength(1)

    await act(async () => { FakeSocket.instances[0].close() })
    // Backoff starts at 250ms.
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(FakeSocket.instances).toHaveLength(2)

    // Events during the gap are unrecoverable, so opening triggers a refresh.
    const fetchMock = globalThis.fetch as ReturnType<typeof vi.fn>
    const before = fetchMock.mock.calls.length
    await act(async () => { FakeSocket.instances[1].onopen?.() })
    await act(async () => { await vi.advanceTimersByTimeAsync(120) })
    expect(fetchMock.mock.calls.length).toBeGreaterThan(before)
  })

  it('backs off further on a repeated drop', async () => {
    stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })

    await act(async () => { FakeSocket.instances[0].close() })
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(FakeSocket.instances).toHaveLength(2)

    await act(async () => { FakeSocket.instances[1].close() })
    // The second wait is longer than the first, so a server that is down does not
    // get hammered at 4 requests a second.
    await act(async () => { await vi.advanceTimersByTimeAsync(300) })
    expect(FakeSocket.instances).toHaveLength(2)
    await act(async () => { await vi.advanceTimersByTimeAsync(400) })
    expect(FakeSocket.instances).toHaveLength(3)
  })

  it('does not reconnect after the view is torn down', async () => {
    stubAPI()
    const view = render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    view.unmount()

    await act(async () => { await vi.advanceTimersByTimeAsync(2000) })
    expect(FakeSocket.instances).toHaveLength(1)
  })

  it('coalesces a burst of events into one refresh', async () => {
    stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    const fetchMock = globalThis.fetch as ReturnType<typeof vi.fn>
    const feedsBefore = fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/messages?')).length

    await act(async () => { for (let index = 0; index < 8; index += 1) FakeSocket.instances[0].emit('NEW_EMAIL') })
    await act(async () => { await vi.advanceTimersByTimeAsync(200) })

    const feedsAfter = fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/messages?')).length
    // A body-fetch backlog emits many events; one refresh answers all of them.
    expect(feedsAfter - feedsBefore).toBe(1)
  })

  it('ignores an event type it does not act on', async () => {
    stubAPI()
    render(<App />)
    await screen.findByRole('button', { name: /第一封/ })
    const fetchMock = globalThis.fetch as ReturnType<typeof vi.fn>
    const before = fetchMock.mock.calls.length

    await act(async () => { FakeSocket.instances[0].emit('SOMETHING_ELSE') })
    await act(async () => { await vi.advanceTimersByTimeAsync(200) })
    expect(fetchMock.mock.calls.length).toBe(before)
  })
})

describe('notify', () => {
  beforeEach(() => { vi.unstubAllGlobals(); notificationsEnabled.current = true })

  it('stays silent when the preference is off', () => {
    const constructor = vi.fn()
    vi.stubGlobal('Notification', Object.assign(constructor, { permission: 'granted' }))
    notificationsEnabled.current = false
    notify()
    expect(constructor).not.toHaveBeenCalled()
  })

  it('stays silent when permission was never granted', () => {
    const constructor = vi.fn()
    vi.stubGlobal('Notification', Object.assign(constructor, { permission: 'default' }))
    notify()
    expect(constructor).not.toHaveBeenCalled()
  })

  it('notifies when allowed', () => {
    const constructor = vi.fn()
    vi.stubGlobal('Notification', Object.assign(constructor, { permission: 'granted' }))
    notify()
    expect(constructor).toHaveBeenCalledWith('NexusMail 收到新邮件')
  })

  it('survives a constructor that throws', () => {
    const constructor = vi.fn(() => { throw new TypeError('illegal constructor') })
    vi.stubGlobal('Notification', Object.assign(constructor, { permission: 'granted' }))
    // A throw here would kill the socket handler and stall every later event.
    expect(() => notify()).not.toThrow()
  })

  it('survives an absent Notification', () => {
    expect('Notification' in window).toBe(false)
    expect(() => notify()).not.toThrow()
  })
})

describe('formatting', () => {
  it('splits address lists on every separator and drops the blanks', () => {
    expect(splitEmails('a@x.com, b@x.com;c@x.com\n\nd@x.com , ')).toEqual(['a@x.com', 'b@x.com', 'c@x.com', 'd@x.com'])
    expect(splitEmails('')).toEqual([])
  })

  it('reduces a sender header to a readable name', () => {
    expect(displaySender('"Ada Lovelace" <ada@example.com>')).toBe('Ada Lovelace')
    // Address only: the address itself is the best name available.
    expect(displaySender('ada@example.com')).toBe('ada@example.com')
    // A header that is nothing but an angle-bracketed address leaves no name, so
    // the raw value stands rather than rendering an empty row.
    expect(displaySender('<ada@example.com>')).toBe('<ada@example.com>')
  })

  it('decodes both encoded-word forms and leaves the rest alone', () => {
    expect(decodeEncodedWords('plain subject')).toBe('plain subject')
    expect(decodeEncodedWords('=?utf-8?B?5L2g5aW9?=')).toBe('你好')
    expect(decodeEncodedWords('=?utf-8?Q?hello=20world?=')).toBe('hello world')
    // Q encoding writes a space as an underscore.
    expect(decodeEncodedWords('=?utf-8?q?a_b?=')).toBe('a b')
    // Undecodable: the raw form is more useful than an empty string.
    expect(decodeEncodedWords('=?utf-8?B?!!!not-base64!!!?=')).toBe('=?utf-8?B?!!!not-base64!!!?=')
    expect(decodeEncodedWords('=?nonsense-charset?B?5L2g?=')).toBe('=?nonsense-charset?B?5L2g?=')
    // A charset with a language suffix is still a charset.
    expect(decodeEncodedWords('=?utf-8*zh?B?5L2g5aW9?=')).toBe('你好')
  })

  it('shows a time for today and a date for anything older', () => {
    const now = new Date()
    expect(formatDate(now.getTime())).toMatch(/\d{2}:\d{2}/)
    const older = new Date(now.getTime() - 40 * 24 * 3600 * 1000)
    expect(formatDate(older.getTime())).not.toMatch(/\d{2}:\d{2}/)
    expect(formatFullDate(Date.UTC(2026, 0, 2, 3, 4))).toContain('2026')
  })

  it('scales byte counts across each unit boundary', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1023)).toBe('1023 B')
    expect(formatBytes(1024)).toBe('1.0 KB')
    expect(formatBytes(1024 * 1024 - 1)).toBe('1024.0 KB')
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB')
  })

  it('names an error and falls back for anything that is not one', () => {
    expect(messageOf(new Error('boom'))).toBe('boom')
    expect(messageOf('boom')).toBe('发生未知错误')
    expect(messageOf(null)).toBe('发生未知错误')
  })
})

describe('message html preparation', () => {
  const attachment = (id: number, contentID?: string) => ({
    id, message_id: 1, filename: `f${id}.png`, content_type: 'image/png',
    content_id: contentID, size_bytes: 10, fetch_state: 'ready',
  })

  it('rewrites a cid: image to the attachment it names', () => {
    const html = prepareMessageHTML('<img src="cid:logo@x">', [attachment(7, '<logo@x>')], 1, false)
    expect(html).toContain('/api/v1/messages/1/attachments/7')
  })

  it('leaves a cid: image alone when no attachment matches', () => {
    const html = prepareMessageHTML('<img src="cid:missing@x">', [attachment(7, '<logo@x>')], 1, false)
    expect(html).toContain('cid:missing@x')
    expect(html).not.toContain('/attachments/')
  })

  it('ignores attachments that carry no content id', () => {
    const html = prepareMessageHTML('<img src="cid:logo@x">', [attachment(7)], 1, false)
    expect(html).toContain('cid:logo@x')
  })

  // The marker attribute ends in "src", so the real src is matched with a boundary
  // rather than as a substring, which the marker itself would satisfy.
  const realSrc = /(^|\s)src="https:\/\/t\.example\/p\.gif"/

  it('marks a blocked remote image and restores it on opt-in', () => {
    const blocked = prepareMessageHTML('<img data-nexusmail-remote-src="https://t.example/p.gif">', [], 1, false)
    expect(blocked).toContain('data-nexusmail-blocked')
    expect(blocked).not.toMatch(realSrc)

    const loaded = prepareMessageHTML('<img data-nexusmail-remote-src="https://t.example/p.gif">', [], 1, true)
    expect(loaded).toMatch(realSrc)
    expect(loaded).not.toContain('data-nexusmail-blocked')
  })

  it('does not mark an element that already carries a src', () => {
    const html = prepareMessageHTML('<img src="cid:x" data-nexusmail-remote-src="https://t.example/p.gif">', [], 1, false)
    expect(html).not.toContain('data-nexusmail-blocked')
  })

  it('leaves a remote marker with no value unloaded', () => {
    const html = prepareMessageHTML('<img data-nexusmail-remote-src="">', [], 1, true)
    // Nothing to load, so it stays a placeholder rather than getting an empty src.
    expect(html).toContain('data-nexusmail-blocked')
  })

  it('rules a table with an explicit border and not one without', () => {
    const ruled = prepareMessageHTML('<table border="1"><tr><td>a</td></tr></table>', [], 1, false)
    expect(ruled).toContain('nexusmail-data')
    expect(ruled).toContain('nexusmail-cell')

    const plain = prepareMessageHTML('<table><tr><td>a</td></tr></table>', [], 1, false)
    expect(plain).not.toContain('nexusmail-data')
  })

  it('treats a border of zero as no border', () => {
    const html = prepareMessageHTML('<table border="0"><tr><td>a</td></tr></table>', [], 1, false)
    expect(html).not.toContain('nexusmail-data')
  })

  it('wraps only the outermost table in a scroll box', () => {
    const html = prepareMessageHTML('<table><tr><td><table><tr><td>a</td></tr></table></td></tr></table>', [], 1, false)
    expect(html.match(/nexusmail-scroll/g)).toHaveLength(1)
  })
})

describe('clipboard fallback', () => {
  beforeEach(() => { vi.unstubAllGlobals() })

  it('refuses an empty value without touching the clipboard', async () => {
    const writeText = vi.fn()
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    await expect(copyText('')).resolves.toBe(false)
    expect(writeText).not.toHaveBeenCalled()
  })

  it('uses the async clipboard when it is available', async () => {
    const writeText = vi.fn(async () => undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    await expect(copyText('482913')).resolves.toBe(true)
    expect(writeText).toHaveBeenCalledWith('482913')
  })

  it('falls back to execCommand when the clipboard refuses', async () => {
    vi.stubGlobal('navigator', { clipboard: { writeText: async () => { throw new Error('denied') } } })
    const execCommand = vi.fn(() => true)
    Object.defineProperty(document, 'execCommand', { value: execCommand, configurable: true })

    await expect(copyText('482913')).resolves.toBe(true)
    expect(execCommand).toHaveBeenCalledWith('copy')
    // The scratch field must not be left behind in the document.
    expect(document.querySelectorAll('textarea')).toHaveLength(0)
  })

  it('reports failure when both paths fail', async () => {
    vi.stubGlobal('navigator', { clipboard: { writeText: async () => { throw new Error('denied') } } })
    Object.defineProperty(document, 'execCommand', { value: () => { throw new Error('unsupported') }, configurable: true })
    await expect(copyText('482913')).resolves.toBe(false)
  })
})
