import { render, screen, waitFor } from '@testing-library/react'
import { act } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

class FakeSocket {
  static instances: FakeSocket[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  closed = false
  constructor(public url: string) { FakeSocket.instances.push(this) }
  close() { this.closed = true }
  emit(type: string) { this.onmessage?.({ data: JSON.stringify({ type, sequence: 1, occurred_at: Date.now(), data: {} }) }) }
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'mail@example.com', display_name: 'Mail', provider: 'qq', status: 'connected' }

function message(id: number, subject: string) {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'preview',
    body_state: 'ready', received_at: Date.now(), is_read: false, is_starred: false, has_attachments: false,
  }
}

describe('realtime mail delivery', () => {
  beforeEach(() => {
    sessionStorage.clear()
    FakeSocket.instances = []
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
  })

  function stubAPI(messages: () => unknown[]) {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/accounts?') || url === '/api/v1/accounts') return json({ items: [account] })
      if (url.includes('/mailboxes')) return json({ items: [] })
      if (url.startsWith('/api/v1/messages')) return json({ items: messages() })
      return json({})
    }))
  }

  it('renders a message pushed over the socket without a manual refresh', async () => {
    let inbox: unknown[] = []
    stubAPI(() => inbox)
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    await act(async () => { FakeSocket.instances[0].onopen?.() })

    inbox = [message(1, 'Instant delivery')]
    await act(async () => { FakeSocket.instances[0].emit('NEW_EMAIL') })

    expect(await screen.findByText('Instant delivery')).toBeInTheDocument()
  })

  // A frame that is not JSON should never arrive: the server writes json.Marshal
  // output and the keepalive is a ping the browser answers without surfacing it
  // here. But an unhandled throw in onmessage drops the resync that frame was
  // about to schedule, so the socket has to keep working across a bad one.
  it('survives a frame that is not JSON and still delivers the next event', async () => {
    let inbox: unknown[] = []
    stubAPI(() => inbox)
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    const socket = FakeSocket.instances[0]
    await act(async () => { socket.onopen?.() })

    await act(async () => { socket.onmessage?.({ data: 'not json at all' }) })
    await act(async () => { socket.onmessage?.({ data: '{"truncated":' }) })

    inbox = [message(1, 'Arrived after the bad frame')]
    await act(async () => { socket.emit('NEW_EMAIL') })

    expect(await screen.findByText('Arrived after the bad frame')).toBeInTheDocument()
    expect(socket.closed).toBe(false)
  })

  // Valid JSON that is not an envelope reaches the type check, which must not
  // throw on a missing type either.
  it('ignores a well-formed frame that carries no event type', async () => {
    stubAPI(() => [])
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    const socket = FakeSocket.instances[0]

    await act(async () => { socket.onmessage?.({ data: 'null' }) })
    await act(async () => { socket.onmessage?.({ data: '{"sequence":1}' }) })

    expect(socket.closed).toBe(false)
  })

  it('keeps a single socket alive across view changes so no event is missed', async () => {
    stubAPI(() => [message(1, 'Existing mail')])
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    expect(await screen.findByText('Existing mail')).toBeInTheDocument()

    // Selecting an account rebuilds the message loader; the socket must survive.
    await act(async () => { screen.getAllByRole('button', { name: /mail@example\.com/ })[0].click() })
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Mail' })).toBeInTheDocument())

    expect(FakeSocket.instances).toHaveLength(1)
    expect(FakeSocket.instances[0].closed).toBe(false)
  })
})
