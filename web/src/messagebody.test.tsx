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
})
