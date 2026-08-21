import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

class FakeSocket {
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  constructor(public url: string) { /* the body tests never drive the socket */ }
  close() { /* no-op */ }
}

function json(payload: unknown) {
  return new Response(JSON.stringify(payload), { status: 200, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'mail@example.com', display_name: 'Mail', provider: 'qq', status: 'connected' }

// The sender is stored as an encoded-word here on purpose: that is the shape rows
// synced before the server-side fix still hold.
const encodedSender = '=?utf-8?q?=E9=98=BF=E9=87=8C=E4=BA=91?= <noreply@aliyun.com>'

const tableBody = '<table width="600" border="1"><tr><th>项目</th></tr>'
  + '<tr><td>阿里云服务器续费</td><td><table><tr><td>嵌套</td></tr></table></td></tr></table>'

function message(overrides: Record<string, unknown> = {}) {
  return {
    id: 1, account_id: 1, direction: 'incoming', subject: '账单', sender: encodedSender,
    recipients: '=?utf-8?B?5rWL6K+V?= <mail@example.com>', from: '[]', to: '[]', cc: '[]', bcc: '[]',
    snippet: 'preview', body_state: 'ready', received_at: 0, is_read: false, is_starred: false,
    has_attachments: false, body_html: tableBody, ...overrides,
  }
}

describe('message body rendering', () => {
  afterEach(cleanup)
  let detail: Record<string, unknown>

  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    detail = message()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/accounts') return json({ items: [account] })
      if (url.includes('/mailboxes')) return json({ items: [] })
      if (/^\/api\/v1\/messages\/\d+$/.test(url)) return json({ message: detail, attachments: [] })
      if (url.startsWith('/api/v1/messages')) return json({ items: [detail], unread_total: 1 })
      return json({})
    }))
  })

  async function openMessage() {
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /账单/ }))
    return await waitFor(() => screen.getByTitle('邮件正文') as HTMLIFrameElement)
  }

  // net/mail re-encoded every non-ASCII display name into an encoded-word, so the
  // sender arrived as a literal "=?utf-8?q?...?=". The server stores the decoded form
  // now; decoding on display is what repairs the rows written before that.
  it('decodes RFC 2047 encoded-words in the sender and recipients', async () => {
    await openMessage()

    expect(screen.getAllByText('阿里云').length).toBeGreaterThan(0)
    expect(screen.queryByText(/=\?utf-8\?q\?/)).not.toBeInTheDocument()
    expect(screen.getByText(/发给 测试/)).toBeInTheDocument()
  })

  it('falls back to the raw value when an encoded-word cannot be decoded', async () => {
    detail = message({ sender: '=?definitely-not-a-charset?q?abc?= <a@b.com>' })
    await openMessage()

    expect(screen.getAllByText(/definitely-not-a-charset/).length).toBeGreaterThan(0)
  })

  // A click used to navigate the frame itself, and because the frame is a sandboxed
  // opaque origin nearly every destination refused to load there. The base target
  // sends links to a new tab; the two popup permissions are what let that tab open
  // and then behave like a normal tab.
  it('opens links in a new tab instead of navigating the sandboxed frame', async () => {
    const frame = await openMessage()

    expect(frame.getAttribute('srcdoc')).toContain('<base target="_blank">')
    const sandbox = frame.getAttribute('sandbox') ?? ''
    expect(sandbox).toContain('allow-popups')
    expect(sandbox).toContain('allow-popups-to-escape-sandbox')
    // The frame still must not run script, reach its own origin or navigate the page.
    expect(sandbox).not.toContain('allow-scripts')
    expect(sandbox).not.toContain('allow-same-origin')
    expect(sandbox).not.toContain('allow-top-navigation')
  })

  // overflow-wrap:anywhere also shrinks a cell's min-content width to one character,
  // which collapsed table-laid-out mail into one-glyph-per-line columns.
  it('wraps at word boundaries so table cells keep their width', async () => {
    const frame = await openMessage()
    const srcDoc = frame.getAttribute('srcdoc') ?? ''

    expect(srcDoc).toContain('overflow-wrap:break-word')
    expect(srcDoc).not.toContain('overflow-wrap:anywhere')
  })

  it('rules data tables, scrolls the outer table and leaves nested tables alone', async () => {
    const frame = await openMessage()
    const srcDoc = frame.getAttribute('srcdoc') ?? ''
    const rendered = new DOMParser().parseFromString(srcDoc, 'text/html')
    const tables = rendered.querySelectorAll('table')

    expect(tables).toHaveLength(2)
    // The outer table declares border="1" and carries a th, so it is a data table.
    expect(tables[0].classList.contains('nexusmail-data')).toBe(true)
    expect(tables[0].parentElement?.className).toBe('nexusmail-scroll')
    // The nested one has neither, and must not get its own scroll box either.
    expect(tables[1].classList.contains('nexusmail-data')).toBe(false)
    expect(rendered.querySelectorAll('.nexusmail-scroll')).toHaveLength(1)
  })

  it('leaves a layout table unruled', async () => {
    detail = message({ body_html: '<table width="600" border="0"><tr><td>纯排版</td></tr></table>' })
    const frame = await openMessage()
    const rendered = new DOMParser().parseFromString(frame.getAttribute('srcdoc') ?? '', 'text/html')

    const table = rendered.querySelector('table')
    expect(table?.classList.contains('nexusmail-data')).toBe(false)
    expect(table?.parentElement?.className).toBe('nexusmail-scroll')
  })

  // The ruling test used querySelector('th'), which searches descendants: a wrapper
  // holding one data table several levels down was ruled as well. In a GitHub
  // notification that marked 7 of 24 tables, and because the cell rule was a
  // descendant selector every layout spacer got a border — a grid of empty boxes.
  it('does not rule a layout table that merely contains a data table', async () => {
    detail = message({
      body_html: '<table><tr><td>'
        + '<table><tr><td><table border="1"><tr><th>状态</th></tr><tr><td>成功</td></tr></table></td></tr></table>'
        + '</td></tr><tr><td></td></tr></table>',
    })
    const frame = await openMessage()
    const rendered = new DOMParser().parseFromString(frame.getAttribute('srcdoc') ?? '', 'text/html')
    const tables = Array.from(rendered.querySelectorAll('table'))

    expect(tables).toHaveLength(3)
    expect(tables[0].classList.contains('nexusmail-data')).toBe(false)
    expect(tables[1].classList.contains('nexusmail-data')).toBe(false)
    expect(tables[2].classList.contains('nexusmail-data')).toBe(true)
    // Only the innermost table's own cells carry the border class, so the two
    // spacer cells of the outer table stay unruled.
    const ruled = Array.from(rendered.querySelectorAll('.nexusmail-cell'))
    expect(ruled).toHaveLength(2)
    expect(ruled.map(cell => cell.textContent)).toEqual(['状态', '成功'])
    expect(ruled.every(cell => cell.closest('table') === tables[2])).toBe(true)
  })

  it('rules only the data table own cells when a table is nested inside it', async () => {
    const frame = await openMessage()
    const rendered = new DOMParser().parseFromString(frame.getAttribute('srcdoc') ?? '', 'text/html')

    // The nested table's single cell belongs to the nested table, not the ruled one.
    const nestedCell = rendered.querySelectorAll('table')[1].querySelector('td')
    expect(nestedCell?.textContent).toBe('嵌套')
    expect(nestedCell?.classList.contains('nexusmail-cell')).toBe(false)
  })

  // A src-less image paints its alt text into the flow. Inside the 16-24px boxes mail
  // uses for status icons that text wraps one glyph per line, which is what turned a
  // notification into a column of stray words.
  it('suppresses the alt text of a blocked remote image until it is loaded', async () => {
    const body = '<img data-nexusmail-remote-src="https://cdn.example.com/icon.png" alt="prepare" width="16" height="16" />'
    detail = message({ body_html: body })
    const frame = await openMessage()
    const srcDoc = frame.getAttribute('srcdoc') ?? ''
    const rendered = new DOMParser().parseFromString(srcDoc, 'text/html')
    const image = rendered.querySelector('img')

    expect(image?.hasAttribute('data-nexusmail-blocked')).toBe(true)
    expect(image?.hasAttribute('src')).toBe(false)
    // The alt survives for assistive tech; only the painted text is hidden.
    expect(image?.getAttribute('alt')).toBe('prepare')
    expect(srcDoc).toContain('img[data-nexusmail-blocked]{visibility:hidden}')

    fireEvent.click(screen.getByText(/点击临时加载/))
    const loaded = await waitFor(() => {
      const next = new DOMParser()
        .parseFromString((screen.getByTitle('邮件正文') as HTMLIFrameElement).getAttribute('srcdoc') ?? '', 'text/html')
        .querySelector('img')
      expect(next?.getAttribute('src')).toBe('https://cdn.example.com/icon.png')
      return next
    })
    expect(loaded?.hasAttribute('data-nexusmail-blocked')).toBe(false)
  })

  it('renders the sender name in the same face as the subject', async () => {
    await openMessage()

    const subject = screen.getByRole('heading', { level: 1, name: '账单' })
    // Both sit in the reading pane header; the sender used the sans-serif body face
    // while the subject used the serif display face.
    const sender = screen.getAllByText('阿里云').find(node => node.className.includes('font-serif'))
    expect(subject.className).toContain('font-serif')
    expect(sender).toBeDefined()
  })
})
