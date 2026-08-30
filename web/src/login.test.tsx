import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { Login } from './components/Login'

// The login form is the only place a credential is entered, so what it does with
// one — and what it refuses to do — is worth pinning: it must not post a key the
// server would reject on length, must not leave the key readable on screen or in
// a URL, and must surface the server's reason rather than a generic failure.

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const validKey = 'a'.repeat(32)
const submitLabel = '进入 NexusMail'

describe('login', () => {
  afterEach(cleanup)

  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    vi.unstubAllGlobals()
  })

  function stubLogin(reply: (body: unknown) => Response | Promise<Response>) {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) !== '/api/v1/auth/session') throw new Error(`unexpected request to ${input}`)
      return reply(JSON.parse(String(init?.body ?? 'null')))
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('masks the key and keeps submission disabled until it is long enough', () => {
    render(<Login onAuthenticated={() => undefined} />)
    const field = screen.getByLabelText('API Key')
    // type=password, so the key is never rendered as readable text.
    expect(field).toHaveAttribute('type', 'password')
    expect(screen.getByRole('button', { name: submitLabel })).toBeDisabled()

    fireEvent.change(field, { target: { value: 'a'.repeat(31) } })
    expect(screen.getByRole('button', { name: submitLabel })).toBeDisabled()

    fireEvent.change(field, { target: { value: validKey } })
    expect(screen.getByRole('button', { name: submitLabel })).toBeEnabled()
  })

  it('posts the key in the body once and reports authentication', async () => {
    const fetchMock = stubLogin(() => json({ csrf_token: 'csrf-value' }, 201))
    const onAuthenticated = vi.fn()
    render(<Login onAuthenticated={onAuthenticated} />)

    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))

    await waitFor(() => expect(onAuthenticated).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    // The key travels in the body, never in the query string, where it would be
    // logged by every proxy on the way.
    expect(String(url)).toBe('/api/v1/auth/session')
    expect(String(url)).not.toContain(validKey)
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({ api_key: validKey })
    // The CSRF token has to be stored, or every later write is rejected.
    expect(sessionStorage.getItem('nexusmail.csrf')).toBe('csrf-value')
  })

  it('shows the reason the server gave and does not report authentication', async () => {
    stubLogin(() => json({ error: { code: 'rate_limited', message: 'too many login attempts' } }, 429))
    const onAuthenticated = vi.fn()
    render(<Login onAuthenticated={onAuthenticated} />)

    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))

    expect(await screen.findByText('too many login attempts')).toBeInTheDocument()
    expect(onAuthenticated).not.toHaveBeenCalled()
    expect(sessionStorage.getItem('nexusmail.csrf')).toBeNull()
    // Still on the form, with the field intact so the key can be corrected.
    expect(screen.getByLabelText('API Key')).toHaveValue(validKey)
  })

  it('re-enables the button after a failure so a mistyped key can be retried', async () => {
    const fetchMock = stubLogin(() => json({ error: { code: 'unauthorized', message: 'invalid API key' } }, 401))
    render(<Login onAuthenticated={() => undefined} />)

    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    expect(await screen.findByText('invalid API key')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
  })

  it('clears the previous error when a second attempt succeeds', async () => {
    let attempt = 0
    stubLogin(() => {
      attempt += 1
      return attempt === 1
        ? json({ error: { code: 'unauthorized', message: 'invalid API key' } }, 401)
        : json({ csrf_token: 'csrf-value' }, 201)
    })
    const onAuthenticated = vi.fn()
    render(<Login onAuthenticated={onAuthenticated} />)

    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    expect(await screen.findByText('invalid API key')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    await waitFor(() => expect(onAuthenticated).toHaveBeenCalled())
    expect(screen.queryByText('invalid API key')).not.toBeInTheDocument()
  })

  it('reports a transport failure instead of failing silently', async () => {
    stubLogin(() => { throw new TypeError('Failed to fetch') })
    render(<Login onAuthenticated={() => undefined} />)

    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))

    expect(await screen.findByText(/Failed to fetch/)).toBeInTheDocument()
  })

  it('asks for notification permission only when the user has not decided', async () => {
    const requestPermission = vi.fn(async () => 'granted' as NotificationPermission)
    vi.stubGlobal('Notification', { permission: 'denied', requestPermission } as unknown as typeof Notification)
    stubLogin(() => json({ csrf_token: 'csrf-value' }, 201))
    const onAuthenticated = vi.fn()
    render(<Login onAuthenticated={onAuthenticated} />)

    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    await waitFor(() => expect(onAuthenticated).toHaveBeenCalled())
    // Already denied: re-prompting is not possible and must not be attempted.
    expect(requestPermission).not.toHaveBeenCalled()

    cleanup()
    Object.assign(Notification, { permission: 'default' })
    render(<Login onAuthenticated={() => undefined} />)
    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    await waitFor(() => expect(requestPermission).toHaveBeenCalledTimes(1))
  })

  // App decides between the form and the mailbox on the stored CSRF token, not on
  // a network probe, so a reload with a live session must not show the form again.
  it('gates the app on the stored session token', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/v1/auth/session' && init?.method === 'POST') return json({ csrf_token: 'csrf-value' }, 201)
      if (url === '/api/v1/accounts') return json({ items: [] })
      if (url.startsWith('/api/v1/messages')) return json({ items: [], unread_total: 0 })
      return json({})
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('WebSocket', class {
      onopen: (() => void) | null = null
      onmessage: ((event: { data: string }) => void) | null = null
      onclose: (() => void) | null = null
      close() { /* no-op */ }
    } as unknown as typeof WebSocket)

    const first = render(<App />)
    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: validKey } })
    fireEvent.click(screen.getByRole('button', { name: submitLabel }))
    await waitFor(() => expect(screen.queryByLabelText('API Key')).not.toBeInTheDocument())
    first.unmount()

    // Same token still in sessionStorage: straight into the mailbox.
    render(<App />)
    expect(screen.queryByLabelText('API Key')).not.toBeInTheDocument()
  })
})
