import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, isAuthenticated, onSessionInvalidated } from './api'

const csrfKey = 'nexusmail.csrf'
const expiryKey = 'nexusmail.session-expires'

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

function unauthorized() {
  return json({ error: { code: 'unauthorized', message: 'invalid session or CSRF token' } }, 401)
}

describe('API session transport', () => {
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    vi.stubGlobal('fetch', vi.fn())
  })

  it('stores CSRF after login and sends it on mutations', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock
      .mockResolvedValueOnce(new Response(JSON.stringify({ csrf_token: 'csrf-value' }), { status: 201, headers: { 'content-type': 'application/json' } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))

    await api.login('x'.repeat(32))
    await api.addAccount({ provider: 'qq' })

    const login = fetchMock.mock.calls[0]
    expect(login[0]).toBe('/api/v1/auth/session')
    expect((login[1]?.headers as Headers).get('X-CSRF-Token')).toBeNull()
    const mutation = fetchMock.mock.calls[1]
    expect((mutation[1]?.headers as Headers).get('X-CSRF-Token')).toBe('csrf-value')
    expect(mutation[1]?.credentials).toBe('include')
  })

  it('shares the CSRF token after login without storing the API key', async () => {
    vi.mocked(fetch).mockResolvedValue(json({ csrf_token: 'new-csrf' }, 201))
    await api.login('x'.repeat(32))

    expect(localStorage.getItem(csrfKey)).toBe('new-csrf')
    expect(sessionStorage.getItem(csrfKey)).toBeNull()
    expect(Object.values(localStorage)).not.toContain('x'.repeat(32))
    expect(isAuthenticated()).toBe(true)
  })

  it('uses the shared token instead of an older tab-local token on mark-read', async () => {
    sessionStorage.setItem(csrfKey, 'old-tab-csrf')
    localStorage.setItem(csrfKey, 'current-cookie-csrf')
    vi.mocked(fetch).mockResolvedValue(json({ is_read: true }))

    await api.patchMessage(7, { is_read: true })

    const [, init] = vi.mocked(fetch).mock.calls[0]
    expect(new Headers(init?.headers).get('X-CSRF-Token')).toBe('current-cookie-csrf')
  })

  it('accepts a legacy tab token until a shared session is established', async () => {
    sessionStorage.setItem(csrfKey, 'legacy-csrf')
    vi.mocked(fetch).mockResolvedValue(json({ is_read: true }))

    expect(isAuthenticated()).toBe(true)
    await api.patchMessage(7, { is_read: true })
    expect(new Headers(vi.mocked(fetch).mock.calls[0][1]?.headers).get('X-CSRF-Token')).toBe('legacy-csrf')
  })

  it.each([
    ['read', () => api.accounts()],
    ['mark-read', () => api.patchMessage(7, { is_read: true })],
    ['bulk mark-read', () => api.markAllRead(new URLSearchParams({ folder: 'inbox' }))],
  ])('clears invalid session state on a 401 from %s without replaying the request', async (_, call) => {
    sessionStorage.setItem(csrfKey, 'old-tab-csrf')
    localStorage.setItem(csrfKey, 'expired-csrf')
    vi.mocked(fetch).mockResolvedValue(unauthorized())

    await expect(call()).rejects.toMatchObject({ status: 401, code: 'unauthorized' })

    expect(isAuthenticated()).toBe(false)
    expect(sessionStorage.getItem(csrfKey)).toBeNull()
    expect(localStorage.getItem(csrfKey)).toBeFalsy()
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it.each([403, 409, 500, 503])('does not discard the session on HTTP %s', async status => {
    localStorage.setItem(csrfKey, 'current-csrf')
    vi.mocked(fetch).mockResolvedValue(json({ error: { code: 'failed', message: 'request failed' } }, status))

    await expect(api.patchMessage(7, { is_read: true })).rejects.toMatchObject({ status })
    expect(localStorage.getItem(csrfKey)).toBe('current-csrf')
  })

  it('keeps login rejection separate from an existing session', async () => {
    localStorage.setItem(csrfKey, 'current-csrf')
    vi.mocked(fetch).mockResolvedValue(json({ error: { code: 'invalid_api_key', message: 'invalid API key' } }, 401))

    await expect(api.login('x'.repeat(32))).rejects.toMatchObject({ status: 401 })
    expect(localStorage.getItem(csrfKey)).toBe('current-csrf')
    expect(new Headers(vi.mocked(fetch).mock.calls[0][1]?.headers).get('X-CSRF-Token')).toBeNull()
  })

  it('does not let a late 401 erase a newer login', async () => {
    sessionStorage.setItem(csrfKey, 'old-csrf')
    let rejectOld!: (response: Response) => void
    vi.mocked(fetch)
      .mockReturnValueOnce(new Promise<Response>(resolve => { rejectOld = resolve }))
      .mockResolvedValueOnce(json({ csrf_token: 'new-csrf' }, 201))
    const oldRequest = api.patchMessage(7, { is_read: true })
    const rejected = expect(oldRequest).rejects.toMatchObject({ status: 401 })

    await api.login('x'.repeat(32))
    rejectOld(unauthorized())
    await rejected

    expect(localStorage.getItem(csrfKey)).toBe('new-csrf')
    expect(isAuthenticated()).toBe(true)
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('clears local authentication even when logout cannot reach the server', async () => {
    sessionStorage.setItem(csrfKey, 'old-tab-csrf')
    localStorage.setItem(csrfKey, 'current-csrf')
    vi.mocked(fetch).mockRejectedValue(new TypeError('Failed to fetch'))

    await expect(api.logout()).rejects.toThrow('Failed to fetch')

    expect(isAuthenticated()).toBe(false)
    expect(sessionStorage.getItem(csrfKey)).toBeNull()
    expect(localStorage.getItem(csrfKey)).toBeFalsy()
  })

  it('does not revive a legacy tab after another tab has logged out', async () => {
    localStorage.setItem(csrfKey, 'current-csrf')
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }))
    await api.logout()

    sessionStorage.setItem(csrfKey, 'legacy-tab-csrf')
    expect(isAuthenticated()).toBe(false)
  })

  // The reported defect. The cookie carries a fixed Expires and is never renewed,
  // so it is gone 12 hours after login while the CSRF token in localStorage stays
  // readable forever. `isAuthenticated()` therefore kept saying yes, the mailbox
  // mounted instead of the login screen, and the first mutation came back 401
  // `authentication required` - surfaced to the user as "标记已读失败".
  it('reports no session once the cookie deadline has passed', async () => {
    vi.mocked(fetch).mockResolvedValue(json({ csrf_token: 'csrf-value', expires_at: Date.now() + 60_000 }, 201))
    await api.login('x'.repeat(32))
    expect(isAuthenticated()).toBe(true)

    localStorage.setItem(expiryKey, String(Date.now() - 1))
    expect(isAuthenticated()).toBe(false)
  })

  it('records the deadline the server set and drops it on logout', async () => {
    const expiresAt = Date.now() + 12 * 60 * 60 * 1000
    vi.mocked(fetch).mockResolvedValue(json({ csrf_token: 'csrf-value', expires_at: expiresAt }, 201))
    await api.login('x'.repeat(32))
    expect(localStorage.getItem(expiryKey)).toBe(String(expiresAt))

    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }))
    await api.logout()
    expect(localStorage.getItem(expiryKey)).toBeNull()
  })

  it('keeps a token stored before deadlines were recorded usable', () => {
    // Upgrading with a tab already logged in leaves a token and no deadline. It has
    // nothing to check against, so it must stay usable until its own 401 clears it.
    localStorage.setItem(csrfKey, 'pre-upgrade-csrf')
    expect(isAuthenticated()).toBe(true)
  })

  it('clears the deadline along with the token on a 401', async () => {
    localStorage.setItem(csrfKey, 'expired-csrf')
    localStorage.setItem(expiryKey, String(Date.now() + 60_000))
    vi.mocked(fetch).mockResolvedValue(unauthorized())

    await expect(api.accounts()).rejects.toMatchObject({ status: 401 })
    expect(localStorage.getItem(expiryKey)).toBeNull()
  })

  // The signal is a window event and App subscribes from an effect, so a 401 that
  // lands before the subscription emptied the token with nobody listening: the
  // mailbox stayed mounted with no session and every later action answered with the
  // server's raw `authentication required`.
  it('reports a session already cleared before the listener subscribed', async () => {
    localStorage.setItem(csrfKey, 'expired-csrf')
    vi.mocked(fetch).mockResolvedValue(unauthorized())
    await expect(api.accounts()).rejects.toMatchObject({ status: 401 })

    const listener = vi.fn()
    const stop = onSessionInvalidated(listener)
    expect(listener).toHaveBeenCalledTimes(1)
    stop()
  })

  it('stays quiet at subscribe time while the session is live', () => {
    localStorage.setItem(csrfKey, 'current-csrf')
    const listener = vi.fn()
    const stop = onSessionInvalidated(listener)
    expect(listener).not.toHaveBeenCalled()
    stop()
  })

  it('reports a session whose deadline passed before the listener subscribed', () => {
    localStorage.setItem(csrfKey, 'current-csrf')
    localStorage.setItem(expiryKey, String(Date.now() - 1))
    const listener = vi.fn()
    const stop = onSessionInvalidated(listener)
    expect(listener).toHaveBeenCalledTimes(1)
    stop()
  })
})
