import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { act } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// The detail pane renders the snapshot taken when the message was opened. A body
// that the provider returns after that — the 202 + "will refresh automatically"
// path, which every message opened before the prefetch reached it takes — has to
// pull the pane forward on its own. The feed refresh cannot: it replaces the list,
// not the open message.

class FakeSocket {
  static instances: FakeSocket[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  closed = false
  constructor(public url: string) { FakeSocket.instances.push(this) }
  close() { this.closed = true }
  emit(type: string, data: unknown = {}) {
    this.onmessage?.({ data: JSON.stringify({ type, sequence: 1, occurred_at: Date.now(), data }) })
  }
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'mail@example.com', display_name: 'Mail', provider: 'qq', status: 'connected' }

function message(id: number, subject: string, bodyState: string, bodyText?: string) {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: '',
    body_state: bodyState, body_text: bodyText, received_at: Date.now(),
    is_read: true, is_starred: false, has_attachments: false,
  }
}

describe('a body that arrives after the message was opened', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    FakeSocket.instances = []
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  // detailRequests counts GETs on the detail endpoint so the test can also assert
  // the pane is not re-fetched for events about other mail.
  function stubAPI(state: () => { body_state: string, body_text?: string }, detailRequests: number[]) {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/accounts?') || url === '/api/v1/accounts') return json({ items: [account] })
      if (url.includes('/mailboxes')) return json({ items: [] })
      if (/\/api\/v1\/messages\/7$/.test(url)) {
        detailRequests.push(1)
        const current = state()
        return json({ message: message(7, 'Slow body', current.body_state, current.body_text), attachments: [] })
      }
      if (url.startsWith('/api/v1/messages')) {
        const current = state()
        return json({ items: [message(7, 'Slow body', current.body_state, current.body_text)] })
      }
      return json({})
    }))
  }

  it('shows the body once MESSAGE_UPDATED names the open message', async () => {
    let current = { body_state: 'fetching', body_text: undefined as string | undefined }
    stubAPI(() => current, [])
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    await act(async () => { screen.getByText('Slow body').click() })
    expect(await screen.findByText(/正文正在从邮件服务商异步获取/)).toBeInTheDocument()

    current = { body_state: 'ready', body_text: 'the body the provider finally returned' }
    await act(async () => { FakeSocket.instances[0].emit('MESSAGE_UPDATED', { message_id: 7 }) })

    expect(await screen.findByText('the body the provider finally returned')).toBeInTheDocument()
    expect(screen.queryByText(/正文正在从邮件服务商异步获取/)).not.toBeInTheDocument()
  })

  it('ignores events about mail that is not the open message', async () => {
    const current = { body_state: 'ready', body_text: 'already here' }
    const detailRequests: number[] = []
    stubAPI(() => current, detailRequests)
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    const row = await screen.findByText('Slow body')
    await act(async () => { row.click() })
    await waitFor(() => expect(detailRequests).toHaveLength(1))

    // A prefetch backlog draining other mail must not re-fetch this pane.
    await act(async () => {
      FakeSocket.instances[0].emit('MESSAGE_UPDATED', { message_id: 8 })
      FakeSocket.instances[0].emit('MESSAGE_UPDATED', { bulk: true, count: 40 })
    })

    expect(detailRequests).toHaveLength(1)
  })
})
