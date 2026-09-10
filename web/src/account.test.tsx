import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AccountDialog } from './components/AccountDialog'

// Connecting an account is the one form that carries a mailbox credential, and the
// provider choice decides whether a credential is even collected: the OAuth
// providers must never be handed a password field, and the password providers must
// never be posted as `oauth2`, which the server rejects on the auth_type CHECK.
//
// The choice is now its own step, so every case starts by picking a service — there
// is no default provider to inherit. The OAuth providers hand off to a popup, and
// what this pins about that is the part a user notices when it breaks: the window
// is opened during the click (a window opened later is a blocked window), a blocked
// window still completes the handshake through a full-page navigation, and the
// outcome is only accepted from our own origin.

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

type PopupStub = { location: { href: string }; closed: boolean; close: () => void }

// A stand-in for the consent window. `closed` is writable so a case can act out the
// user dismissing it, which is the only way that end of the flow reports anything —
// there is no event for a closed popup.
function stubPopup(handle: PopupStub = { location: { href: '' }, closed: false, close: vi.fn() }) {
  const open = vi.fn(() => handle)
  vi.stubGlobal('open', open)
  return { handle, open }
}

// What a popup blocker leaves behind: window.open returns null.
function stubBlockedPopup() {
  const open = vi.fn(() => null)
  vi.stubGlobal('open', open)
  return open
}

function stubLocationHref() {
  const assigned: string[] = []
  vi.stubGlobal('location', {
    get href() { return 'http://localhost/' },
    set href(value: string) { assigned.push(value) },
    origin: 'http://localhost:3000',
    search: '',
  })
  return assigned
}

// The picker card carries the brand label and its hint, so it is matched on the
// label prefix rather than an exact name.
function pick(label: string) {
  fireEvent.click(screen.getByRole('button', { name: new RegExp(`^${label}\\b`) }))
}

// On step two the chip row and the picker never coexist, so an exact-name lookup
// resolves to the chip.
function chip(label: string) {
  return screen.getByRole('button', { name: label })
}

function fill(fields: { name?: string; email?: string; password?: string }) {
  if (fields.name !== undefined) fireEvent.change(screen.getByLabelText('显示名称'), { target: { value: fields.name } })
  if (fields.email !== undefined) fireEvent.change(screen.getByLabelText('邮箱地址'), { target: { value: fields.email } })
  if (fields.password !== undefined) fireEvent.change(screen.getByLabelText('授权码'), { target: { value: fields.password } })
}

function submit() {
  // The submit button's label depends on the path ('继续' / '使用网页授权'), so the
  // form itself is the stable handle.
  const form = document.querySelector('form')
  if (!form) throw new Error('no form rendered')
  fireEvent.submit(form)
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

    pick('QQ')
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

    pick('163')
    fill({ name: '备用', email: 'me@163.com', password: 'code' })
    submit()

    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0].body).toMatchObject({ provider: '163', auth: { type: 'password' } })
  })

  it('never shows a credential field for the OAuth providers', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Gmail')
    for (const label of ['Gmail', 'Outlook', 'Hotmail']) {
      fireEvent.click(chip(label))
      expect(screen.queryByLabelText('授权码')).not.toBeInTheDocument()
      expect(screen.queryByLabelText('邮箱地址')).not.toBeInTheDocument()
      expect(screen.getByText(/授权窗口/)).toBeInTheDocument()
    }

    // Switching to a password provider restores them, so a mis-click is recoverable
    // without leaving the form.
    fireEvent.click(chip('QQ'))
    expect(screen.getByLabelText('授权码')).toBeInTheDocument()
    expect(screen.queryByText(/授权窗口/)).not.toBeInTheDocument()
  })

  it('opens the consent window during the click and navigates it once the URL arrives', async () => {
    const posts = stubAddAccount(() => json({ authorization_url: 'https://accounts.google.com/o/oauth2/v2/auth?state=abc' }, 202))
    const onCreated = vi.fn()
    const assigned = stubLocationHref()
    const { handle, open } = stubPopup()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    pick('Gmail')
    fill({ name: 'Gmail' })
    submit()

    // Opened blank and synchronously: a window opened after the POST resolves is
    // one the browser did not attribute to a gesture, and gets blocked.
    expect(open).toHaveBeenCalledTimes(1)
    expect(open).toHaveBeenCalledWith('', 'nexusmail-oauth', expect.stringContaining('width=520'))
    await waitFor(() => expect(handle.location.href).toBe('https://accounts.google.com/o/oauth2/v2/auth?state=abc'))
    // No credential fields exist on this path, so nothing but the display name is
    // offered to the server.
    expect(posts[0].body).toEqual({ provider: 'gmail', display_name: 'Gmail', auth: { type: 'oauth2' } })
    // The page itself must not move, and the account does not exist yet.
    expect(assigned).toEqual([])
    expect(onCreated).not.toHaveBeenCalled()
    expect(await screen.findByRole('button', { name: '等待授权完成…' })).toBeInTheDocument()
  })

  it('falls back to a full-page navigation when the consent window is blocked', async () => {
    stubAddAccount(() => json({ authorization_url: 'https://login.microsoftonline.com/common/oauth2/v2.0/authorize?state=abc' }, 202))
    const assigned = stubLocationHref()
    stubBlockedPopup()
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Outlook')
    submit()

    // A blocked popup must not strand the user: the handshake still completes, it
    // just costs the open mailbox, which is what this flow used to do every time.
    await waitFor(() => expect(assigned).toEqual(['https://login.microsoftonline.com/common/oauth2/v2.0/authorize?state=abc']))
  })

  it('posts Hotmail as the outlook provider on the OAuth path', async () => {
    const posts = stubAddAccount(() => json({ authorization_url: 'https://login.microsoftonline.com/x' }, 202))
    stubLocationHref()
    stubPopup()
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Hotmail')
    submit()

    // Hotmail is Outlook: the same Microsoft endpoint and the same preset. It is a
    // separate chip because a user with that address will look for that word, but
    // `outlook` is the only value accounts.provider accepts for it.
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0].body).toMatchObject({ provider: 'outlook', auth: { type: 'oauth2' } })
  })

  it('finishes when the consent window reports success from this origin', async () => {
    stubAddAccount(() => json({ authorization_url: 'https://accounts.google.com/x' }, 202))
    const onCreated = vi.fn()
    const { handle } = stubPopup()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    pick('Gmail')
    submit()
    await waitFor(() => expect(handle.location.href).toBe('https://accounts.google.com/x'))

    // A message from somewhere else must not be able to announce an account that
    // was never created.
    window.dispatchEvent(new MessageEvent('message', { data: { source: 'nexusmail-oauth', status: 'success' }, origin: 'https://evil.example.com' }))
    expect(onCreated).not.toHaveBeenCalled()

    window.dispatchEvent(new MessageEvent('message', { data: { source: 'nexusmail-oauth', status: 'success' }, origin: location.origin }))
    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(handle.close).toHaveBeenCalled()
  })

  it('reports the reason and allows a retry when authorization fails', async () => {
    stubAddAccount(() => json({ authorization_url: 'https://accounts.google.com/x' }, 202))
    const onCreated = vi.fn()
    const { handle } = stubPopup()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    pick('Gmail')
    submit()
    await waitFor(() => expect(handle.location.href).toBe('https://accounts.google.com/x'))

    window.dispatchEvent(new MessageEvent('message', { data: { source: 'nexusmail-oauth', status: 'error', reason: 'access_denied' }, origin: location.origin }))

    expect(await screen.findByText(/access_denied/)).toBeInTheDocument()
    expect(onCreated).not.toHaveBeenCalled()
    // The wait has to be released, or the dialog is a dead end.
    expect(screen.getByRole('button', { name: '使用网页授权' })).toBeEnabled()
  })

  it('releases the wait when the user closes the consent window', async () => {
    stubAddAccount(() => json({ authorization_url: 'https://accounts.google.com/x' }, 202))
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const popup = { location: { href: '' }, closed: false, close: vi.fn() }
    stubPopup(popup)
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Gmail')
    submit()
    await waitFor(() => expect(popup.location.href).toBe('https://accounts.google.com/x'))

    // Dismissing the consent window fires nothing, so the dialog has to notice.
    popup.closed = true
    await vi.advanceTimersByTimeAsync(600)
    expect(await screen.findByText('授权窗口已关闭，请重试。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '使用网页授权' })).toBeEnabled()
    vi.useRealTimers()
  })

  it('finishes locally when a password provider returns an account rather than a redirect', async () => {
    stubAddAccount(() => json({ id: 3, provider: 'qq' }, 201))
    const assigned = stubLocationHref()
    const { open } = stubPopup()
    const onCreated = vi.fn()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    pick('QQ')
    fill({ name: '工作', email: 'me@qq.com', password: 'code' })
    submit()

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(assigned).toEqual([])
    // The password path never opens a window; one would be a popup the user did
    // not ask for.
    expect(open).not.toHaveBeenCalled()
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

    pick('QQ')
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
    expect(posts[1].body).toMatchObject({ auth: { password: 'right' } })
  })

  it('reports a transport failure instead of leaving the dialog silent', async () => {
    stubAddAccount(() => { throw new TypeError('Failed to fetch') })
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('QQ')
    fill({ name: '工作', email: 'me@qq.com', password: 'code' })
    submit()

    expect(await screen.findByText(/Failed to fetch/)).toBeInTheDocument()
  })

  it('requires an address and a credential on the password path', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('QQ')
    // The browser blocks the submit; the fields carry the constraint that says so.
    expect(screen.getByLabelText('邮箱地址')).toBeRequired()
    expect(screen.getByLabelText('授权码')).toBeRequired()
    expect(screen.getByLabelText('授权码')).toHaveAttribute('type', 'password')
    expect(screen.getByLabelText('邮箱地址')).toHaveAttribute('type', 'email')
    // The display name is cosmetic and must not block the connection.
    expect(screen.getByLabelText('显示名称')).not.toBeRequired()
  })

  it('offers every provider on the picker before anything is chosen', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    // The brands are written the way the brands write themselves — the row used to
    // be CSS-uppercased into GMAIL and OUTLOOK.
    for (const label of ['QQ', '163', 'Gmail', 'Outlook', 'Hotmail']) {
      expect(screen.getByRole('button', { name: new RegExp(`^${label}\\b`) })).toBeInTheDocument()
    }
    // No form until a service is chosen: the fields depend on which one it is.
    expect(screen.queryByLabelText('显示名称')).not.toBeInTheDocument()
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

    pick('QQ')
    const labels = ['QQ', '163', 'Gmail', 'Outlook', 'Hotmail']
    const selected = () => labels.filter(label => chip(label).getAttribute('aria-pressed') === 'true')

    expect(selected()).toEqual(['QQ'])
    fireEvent.click(chip('Outlook'))
    expect(selected()).toEqual(['Outlook'])
    // Outlook and Hotmail post the same provider but are distinct choices, so
    // selecting one must not light up the other.
    fireEvent.click(chip('Hotmail'))
    expect(selected()).toEqual(['Hotmail'])
  })

  it('returns to the picker and drops the chosen service', () => {
    stubAddAccount(() => json({}, 201))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('QQ')
    expect(screen.getByLabelText('授权码')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '返回' }))
    expect(screen.queryByLabelText('授权码')).not.toBeInTheDocument()
    expect(screen.getByText('选择邮箱服务商')).toBeInTheDocument()
  })
})
