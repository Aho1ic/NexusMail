import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { loadPreferences } from './lib/preferences'

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

// A body carrying the marker the server writes for every blocked remote image.
const remoteImageBody = '<p>hello</p><img data-nexusmail-remote-src="https://tracker.example.com/pixel.gif" />'

function message(id: number, subject: string) {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'preview',
    body_state: 'ready', body_html: remoteImageBody, received_at: Date.now(),
    is_read: true, is_starred: false, has_attachments: false,
  }
}

const inbox = [message(1, 'First mail'), message(2, 'Second mail')]

function stubAPI(accounts: unknown[] = [account]) {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url === '/api/v1/accounts') return json({ items: accounts })
    if (url.includes('/mailboxes')) return json({ items: [] })
    if (/^\/api\/v1\/messages\/\d+$/.test(url)) {
      const id = Number(url.split('/').pop())
      return json({ message: inbox.find(item => item.id === id), attachments: [] })
    }
    if (url.startsWith('/api/v1/messages')) return json({ items: inbox })
    return json({})
  }))
}

describe('settings', () => {
  // vitest runs without globals, so testing-library's automatic teardown never
  // registers and each render would otherwise stack up in the same document.
  afterEach(cleanup)

  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    stubAPI()
  })

  async function openSettings() {
    render(<App />)
    expect(await screen.findByRole('button', { name: /First mail/ })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '设置' }))
    return screen.findByRole('dialog', { name: '设置' })
  }

  it('opens from the sidebar and persists a toggled preference', async () => {
    await openSettings()
    const toggle = screen.getByRole('switch', { name: '自动加载远程图片' })
    expect(toggle).toHaveAttribute('aria-checked', 'false')

    fireEvent.click(toggle)

    expect(toggle).toHaveAttribute('aria-checked', 'true')
    expect(loadPreferences().autoLoadRemoteImages).toBe(true)
  })

  it('blocks remote images by default and loads them once the setting is on', async () => {
    await openSettings()
    fireEvent.click(screen.getByRole('button', { name: '完成' }))
    fireEvent.click(screen.getByRole('button', { name: /First mail/ }))
    expect(await screen.findByRole('button', { name: /已阻止的远程图片/ })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '设置' }))
    fireEvent.click(await screen.findByRole('switch', { name: '自动加载远程图片' }))
    fireEvent.click(screen.getByRole('button', { name: '完成' }))

    await waitFor(() => expect(screen.queryByRole('button', { name: /已阻止的远程图片/ })).not.toBeInTheDocument())
  })

  it('stops the single-key shortcuts from firing when disabled', async () => {
    await openSettings()
    fireEvent.click(screen.getByRole('switch', { name: '启用单键快捷键' }))
    fireEvent.click(screen.getByRole('button', { name: '完成' }))

    fireEvent.keyDown(window, { key: 'j' })

    // The reading pane must still show the placeholder instead of a message.
    expect(await screen.findByRole('heading', { name: '收件箱已就绪' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'First mail' })).not.toBeInTheDocument()
    expect(loadPreferences().keyboardShortcuts).toBe(false)
  })

  it('keeps the shortcuts working while enabled', async () => {
    await openSettings()
    fireEvent.click(screen.getByRole('button', { name: '完成' }))

    fireEvent.keyDown(window, { key: 'j' })

    expect(await screen.findByRole('heading', { name: 'First mail' })).toBeInTheDocument()
  })

  it('reports the account sync error the gateway last recorded', async () => {
    stubAPI([{ ...account, status: 'backoff', last_error: 'sync Drafts: connection reset' }])
    const dialog = await openSettings()

    expect(dialog).toHaveTextContent('连接异常，正在重试')
    expect(dialog).toHaveTextContent('sync Drafts: connection reset')
  })
})
