import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api'

describe('API session transport', () => {
  beforeEach(() => {
    sessionStorage.clear()
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
})
