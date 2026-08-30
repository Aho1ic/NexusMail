import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AccountDialog } from './components/AccountDialog'

// Connecting an account is the one form that carries a mailbox credential, and the
// provider choice decides whether a credential is even collected: the OAuth
// providers must never be handed a password field, and the password providers must
// never be posted as `oauth2`, which the server rejects on the auth_type CHECK.

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

type Posted = { url: string; method: string; csrf: string | null; body: Record<string, unknown> }

function stubAddAccount(reply: (posted: Posted) => Response) {
  const posts: Posted[] = []
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const posted: Posted = {
      url: String(input),
      method: (init?.method ?? 'GET').toUpperCase(),
      csrf: new Headers(init?.headers).get('X-CSRF-Token'),
      body: JSON.parse(String(init?.body ?? '{}')),
    }
    posts.push(posted)
    return reply(posted)
  })
  vi.stubGlobal('fetch', fetchMock)
  return posts
}

function fill(fields: { name?: string; email?: string; password?: string }) {
  if (fields.name !== undefined) fireEvent.change(screen.getByLabelText('显示名称'), { target: { value: fields.name } })
  if (fields.email !== undefined) fireEvent.change(screen.getByLabelText('邮箱地址'), { target: { value: fields.email } })
  if (fields.password !== undefined) fireEvent.change(screen.getByLabelText('授权码'), { target: { value: fields.password } })
}

function submit() {
  fireEvent.submit(screen.getByRole('button', { name: '继续' }).closest('form') as HTMLFormElement)
}

describe('account dialog', () => {
  afterEach(cleanup)

  beforeEach(() => {
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  it('collects a credential for the password providers and posts auth.type password', async () => {
    const posts = stubAddAccount(() => json({ id: 1, email: 'me@qq.com', display_name: '工作', provider: 'qq', status: 'connecting' }, 201))
    const onCreated = vi.fn()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    // qq is the default, so the credential fields are present without a click.
    fill({ name: '工作', email: 'me@qq.com', password: 'app-specific-code' })
    submit()

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(posts).toHaveLength(1)
    expect(posts[0].url).toBe('/api/v1/accounts')
    expect(posts[0].method).toBe('POST')
    expect(posts[0].csrf).toBe('csrf-value')
    expect(posts[0].body).toEqual({
      provider: 'qq', email: 'me@qq.com', display_name: '工作', username: 'me@qq.com',
      // The username defaults to the address: the presets authenticate with the
      // full address, not a local part.
      auth: { type: 'password', password: 'app-specific-code' },
    })
  })

  it('keeps 163 on the password path', async () => {
    const posts = stubAddAccount(() => json({ id: 2 }, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    fireEvent.click(screen.getByRole('button', { name: '163' }))
    fill({ name: '备用', email: 'me@163.com', password: 'code' })
    submit()

    await waitFor(() => expect(posts).toHaveLength(1))
    expect((posts[0].body.auth as { type: string }).type).toBe('password')
  })

  it('never shows a credential field for the OAuth providers', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    for (const provider of ['gmail', 'outlook']) {
      fireEvent.click(screen.getByRole('button', { name: provider }))
      expect(screen.queryByLabelText('授权码')).not.toBeInTheDocument()
      expect(screen.queryByLabelText('邮箱地址')).not.toBeInTheDocument()
      expect(screen.getByText(/跳转到服务商授权页面/)).toBeInTheDocument()
    }

    // Switching back restores them, so a mis-click is recoverable.
    fireEvent.click(screen.getByRole('button', { name: 'qq' }))
    expect(screen.getByLabelText('授权码')).toBeInTheDocument()
  })

  it('posts auth.type oauth2 and follows the authorization URL instead of finishing locally', async () => {
    const posts = stubAddAccount(() => json({ authorization_url: 'https://accounts.google.com/o/oauth2/v2/auth?state=abc' }, 202))
    const onCreated = vi.fn()
    const assigned: string[] = []
    vi.stubGlobal('location', { get href() { return 'http://localhost/' }, set href(value: string) { assigned.push(value) } })
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    fireEvent.click(screen.getByRole('button', { name: 'gmail' }))
    fill({ name: 'Gmail' })
    submit()

    await waitFor(() => expect(assigned).toEqual(['https://accounts.google.com/o/oauth2/v2/auth?state=abc']))
    expect((posts[0].body.auth as { type: string }).type).toBe('oauth2')
    // The account does not exist yet; announcing it would refresh an empty list
    // and hide the redirect.
    expect(onCreated).not.toHaveBeenCalled()
  })

  it('finishes locally when a password provider returns an account rather than a redirect', async () => {
    stubAddAccount(() => json({ id: 3, provider: 'qq' }, 201))
    const assigned: string[] = []
    vi.stubGlobal('location', { get href() { return 'http://localhost/' }, set href(value: string) { assigned.push(value) } })
    const onCreated = vi.fn()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    fill({ name: '工作', email: 'me@qq.com', password: 'code' })
    submit()

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(assigned).toEqual([])
  })

  it('surfaces the server reason and lets the form be resubmitted', async () => {
    let attempt = 0
    const posts = stubAddAccount(() => {
      attempt += 1
      return attempt === 1
        ? json({ error: { code: 'unauthorized', message: '授权码校验失败' } }, 401)
        : json({ id: 4 }, 201)
    })
    const onCreated = vi.fn()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    fill({ name: '工作', email: 'me@qq.com', password: 'wrong' })
    submit()

    expect(await screen.findByText('授权码校验失败')).toBeInTheDocument()
    expect(onCreated).not.toHaveBeenCalled()
    // busy has to be released on failure, or the dialog is a dead end.
    expect(screen.getByRole('button', { name: '继续' })).toBeEnabled()

    fill({ password: 'right' })
    submit()
    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(posts).toHaveLength(2)
    expect((posts[1].body.auth as { password: string }).password).toBe('right')
  })

  it('reports a transport failure instead of leaving the dialog silent', async () => {
    stubAddAccount(() => { throw new TypeError('Failed to fetch') })
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    fill({ name: '工作', email: 'me@qq.com', password: 'code' })
    submit()

    expect(await screen.findByText(/Failed to fetch/)).toBeInTheDocument()
  })

  it('requires an address and a credential on the password path', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)
    // The browser blocks the submit; the fields carry the constraint that says so.
    expect(screen.getByLabelText('邮箱地址')).toBeRequired()
    expect(screen.getByLabelText('授权码')).toBeRequired()
    expect(screen.getByLabelText('授权码')).toHaveAttribute('type', 'password')
    expect(screen.getByLabelText('邮箱地址')).toHaveAttribute('type', 'email')
    // The display name is cosmetic and must not block the connection.
    expect(screen.getByLabelText('显示名称')).not.toBeRequired()
  })

  it('closes without posting anything', () => {
    const posts = stubAddAccount(() => json({}, 201))
    const onClose = vi.fn()
    render(<AccountDialog onClose={onClose} onCreated={() => undefined} />)

    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    expect(onClose).toHaveBeenCalledTimes(1)
    expect(posts).toHaveLength(0)
  })

  it('marks the selected provider and leaves only one selected', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)
    const selected = () => ['qq', '163', 'gmail', 'outlook']
      .filter(item => screen.getByRole('button', { name: item }).className.includes('border-pine'))

    expect(selected()).toEqual(['qq'])
    fireEvent.click(screen.getByRole('button', { name: 'outlook' }))
    expect(selected()).toEqual(['outlook'])
  })
})
