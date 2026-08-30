import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

class FakeSocket {
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  constructor(public url: string) { /* no-op */ }
  close() { /* no-op */ }
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'mail@example.com', display_name: 'Mail', provider: 'qq', status: 'connected' }

function draft(id: number, revision: number) {
  return {
    id, account_id: 1, to: '[]', cc: '[]', bcc: '[]', subject: '', body_text: '',
    status: 'draft', remote_sync_state: 'dirty', revision, attempt_count: 0,
    created_at: 0, updated_at: 0,
  }
}

describe('the composer persists exactly one draft', () => {
  afterEach(cleanup)

  let created: number
  let updates: string[]
  let sent: number[]
  // Resolves the pending createDraft response, so a test can hold the create in
  // flight while it types again — which is what produced two drafts.
  let releaseCreate: (() => void) | null

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    sessionStorage.clear()
    localStorage.clear()
    created = 0
    updates = []
    sent = []
    releaseCreate = null
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/accounts') return json({ items: [account] })
      if (url.includes('/mailboxes')) return json({ items: [] })
      if (url === '/api/v1/drafts' && method === 'POST') {
        created++
        const id = 100 + created
        if (releaseCreate) {
          await new Promise<void>(resolve => { releaseCreate = resolve })
        }
        return json(draft(id, 1), 201)
      }
      const patch = url.match(/^\/api\/v1\/drafts\/(\d+)$/)
      if (patch && method === 'PATCH') {
        const revision = Number(new Headers(init?.headers).get('If-Match')?.replace(/"/g, '') ?? '0')
        updates.push(`${patch[1]}@${revision}`)
        return json(draft(Number(patch[1]), revision + 1))
      }
      const send = url.match(/^\/api\/v1\/drafts\/(\d+)\/send$/)
      if (send) { sent.push(Number(send[1])); return json({ id: Number(send[1]), status: 'queued' }, 202) }
      if (url.startsWith('/api/v1/messages')) return json({ items: [], unread_total: 0 })
      return json({})
    }))
  })

  afterEach(() => vi.useRealTimers())

  async function openComposer() {
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: '写邮件' }))
    return await screen.findByLabelText('收件人')
  }

  it('creates one draft and then updates it across consecutive edits', async () => {
    const to = await openComposer()
    fireEvent.change(to, { target: { value: 'a@example.com' } })

    // First autosave: a create.
    await vi.advanceTimersByTimeAsync(2100)
    await waitFor(() => expect(created).toBe(1))

    // Second autosave after another edit must be an update of the same draft.
    fireEvent.change(to, { target: { value: 'b@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)
    await waitFor(() => expect(updates).toEqual(['101@1']))
    expect(created).toBe(1)
  })

  it('does not create a second draft when an edit lands while the create is in flight', async () => {
    // This is the regression: the autosave closure captured draft === null, so a
    // second edit that fired before the first create resolved POSTed again and
    // orphaned the first draft on the server and on the provider.
    releaseCreate = () => undefined
    const to = await openComposer()
    fireEvent.change(to, { target: { value: 'a@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)
    await waitFor(() => expect(created).toBe(1))

    // The create is now parked. Type again and let the next autosave fire.
    fireEvent.change(to, { target: { value: 'ab@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)

    // Release the create; the queued save must resolve into an update.
    const release = releaseCreate as unknown as () => void
    release()
    await waitFor(() => expect(updates).toEqual(['101@1']))
    expect(created).toBe(1)
  })

  it('sends the draft it just saved rather than creating another one', async () => {
    const to = await openComposer()
    fireEvent.change(to, { target: { value: 'a@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)
    await waitFor(() => expect(created).toBe(1))

    fireEvent.change(to, { target: { value: 'c@example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /发送/ }))

    await waitFor(() => expect(sent).toEqual([101]))
    expect(created).toBe(1)
    expect(updates).toEqual(['101@1'])
  })

  it('sends without a prior autosave by creating the draft once', async () => {
    const to = await openComposer()
    fireEvent.change(to, { target: { value: 'a@example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /发送/ }))

    await waitFor(() => expect(sent).toEqual([101]))
    // The pending autosave must not fire a second create after the send.
    await vi.advanceTimersByTimeAsync(3000)
    expect(created).toBe(1)
  })
})
