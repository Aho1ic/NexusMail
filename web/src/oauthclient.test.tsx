import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AccountDialog } from './components/AccountDialog'
import { SettingsDialog } from './components/SettingsDialog'
import { defaultPreferences } from './lib/preferences'

// The failure this covers was reported from a real container: a new Outlook account,
// a press of 使用网页授权, and back came "missing Microsoft OAuth client credentials"
// — the deployment had never set NEXUSMAIL_MICROSOFT_CLIENT_ID. Three things follow
// from that, and each is pinned here.
//
// The credentials are configurable from the page, so the repair happens in the window
// that reported the problem rather than in a file beside the compose file.
//
// The dialog only withdraws 使用网页授权 on an explicit `configured: false`. A probe
// that fails, 500s, or has not answered yet leaves the button exactly where it was:
// it is the working path on every correctly deployed gateway, and hiding it because a
// status call did not land would break the common case to warn about the rare one.
//
// And there is a way through that never needs the browser to come back at all: the
// user opens the consent URL in a tab and pastes the code — or the whole callback
// address — into the dialog.

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

type Call = { url: string; method: string; body: Record<string, unknown> }

const gmailStatus = {
  provider: 'gmail', configured: true, source: 'environment' as const,
  client_id: '123.apps.googleusercontent.com',
  redirect_uri: 'http://localhost:13737/api/v1/oauth/gmail/callback',
  env_client_id_key: 'NEXUSMAIL_GOOGLE_CLIENT_ID', env_client_secret_key: 'NEXUSMAIL_GOOGLE_CLIENT_SECRET',
}

const outlookMissing = {
  provider: 'outlook', configured: false, source: 'none' as const, client_id: '',
  redirect_uri: 'http://localhost:13737/api/v1/oauth/outlook/callback',
  env_client_id_key: 'NEXUSMAIL_MICROSOFT_CLIENT_ID', env_client_secret_key: 'NEXUSMAIL_MICROSOFT_CLIENT_SECRET',
}

// One recorder for every call the dialogs make. `reply` answers everything the
// default does not, so a case only spells out the endpoint it is about.
function stubAPI(reply: (call: Call) => Response | null = () => null) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = {
      url: String(input),
      method: (init?.method ?? 'GET').toUpperCase(),
      body: init?.body ? JSON.parse(String(init.body)) : {},
    }
    calls.push(call)
    return reply(call) ?? json({})
  }))
  return calls
}

function pick(label: string) {
  fireEvent.click(screen.getByRole('button', { name: new RegExp(`^${label}\\b`) }))
}

const webAuth = () => screen.queryByRole('button', { name: '使用网页授权' })

describe('oauth client configuration', () => {
  afterEach(cleanup)

  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  it('replaces the authorization button with the client form when the gateway holds no client', async () => {
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [gmailStatus, outlookMissing] })
      if (call.url === '/api/v1/oauth/clients/outlook' && call.method === 'PUT') {
        return json({ ...outlookMissing, configured: true, source: 'database', client_id: 'app-id', updated_at: 1757000000000 })
      }
      return null
    })
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    // The picker says so before a click is spent on it.
    expect(await screen.findByRole('button', { name: /^Outlook需先配置 OAuth Client/ })).toBeInTheDocument()
    // Gmail's client is configured, so its own promise stands.
    expect(screen.getByRole('button', { name: /^Gmail使用网页授权登录/ })).toBeInTheDocument()

    pick('Outlook')
    // Offering 使用网页授权 here is the misleading part: it cannot work. The
    // paste-the-code path is the same authorization and needs the same client, so
    // it stays hidden until the pair is saved.
    expect(webAuth()).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '手动输入授权码' })).not.toBeInTheDocument()
    expect(screen.getByText(/缺少 Outlook 的 OAuth Client/)).toBeInTheDocument()
    // The environment path stays documented, by the exact variable names the .env
    // beside the compose file wants.
    expect(screen.getByText('NEXUSMAIL_MICROSOFT_CLIENT_ID')).toBeInTheDocument()
    expect(screen.getByText('NEXUSMAIL_MICROSOFT_CLIENT_SECRET')).toBeInTheDocument()
    // And the redirect URI, which has to be registered with the provider verbatim.
    expect(screen.getByText('http://localhost:13737/api/v1/oauth/outlook/callback')).toBeInTheDocument()

    fireEvent.change(screen.getByLabelText('Outlook Client ID'), { target: { value: 'app-id' } })
    const secret = screen.getByLabelText('Outlook Client Secret')
    // Write-only: the server never returns it, so there is nothing to prefill.
    expect(secret).toHaveValue('')
    expect(secret).toHaveAttribute('type', 'password')
    fireEvent.change(secret, { target: { value: 'app-secret' } })
    fireEvent.click(screen.getByRole('button', { name: '保存' }))

    // Saved, and the button the whole flow depends on comes back in place.
    expect(await screen.findByRole('button', { name: '使用网页授权' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '手动输入授权码' })).toBeInTheDocument()
    const put = calls.find(call => call.method === 'PUT')
    expect(put).toMatchObject({ url: '/api/v1/oauth/clients/outlook', body: { client_id: 'app-id', client_secret: 'app-secret' } })
    // The form goes with the warning it lived in: there is nothing left to fix.
    expect(screen.queryByLabelText('Outlook Client ID')).not.toBeInTheDocument()
    // Nothing was posted to /accounts: saving a client is not connecting a mailbox.
    expect(calls.some(call => call.url === '/api/v1/accounts')).toBe(false)
  })

  it('leaves both fields required before a save is attempted', async () => {
    stubAPI(call => call.url === '/api/v1/oauth/clients' ? json({ items: [outlookMissing] }) : null)
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)
    await screen.findByRole('button', { name: /^Outlook需先配置 OAuth Client/ })

    pick('Outlook')
    // A half-filled pair is refused by the server, so it is not offered here: the
    // secret cannot be omitted to mean "keep the old one" — there is no old one to
    // keep on this path.
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Outlook Client ID'), { target: { value: 'app-id' } })
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Outlook Client Secret'), { target: { value: 'app-secret' } })
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled()
  })

  it('reports a refused save without losing the typed client id', async () => {
    stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [outlookMissing] })
      if (call.method === 'PUT') return json({ error: { code: 'invalid_request', message: 'client_id is required' } }, 400)
      return null
    })
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)
    await screen.findByRole('button', { name: /^Outlook需先配置 OAuth Client/ })

    pick('Outlook')
    fireEvent.change(screen.getByLabelText('Outlook Client ID'), { target: { value: 'app-id' } })
    fireEvent.change(screen.getByLabelText('Outlook Client Secret'), { target: { value: 'app-secret' } })
    fireEvent.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('client_id is required')
    // Retryable: the button releases and the id the user typed is still there.
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled()
    expect(screen.getByLabelText('Outlook Client ID')).toHaveValue('app-id')
  })

  // The hard requirement. A gateway that cannot answer the probe is overwhelmingly a
  // gateway whose OAuth client is configured — an older build with no such route, or
  // one call that happened to fail — and withdrawing the button there would break
  // the flow for everyone to warn the few.
  it.each([
    { name: 'the probe fails outright', reply: null as (() => Response) | null },
    { name: 'the probe answers 500', reply: () => json({ error: { code: 'internal', message: 'boom' } }, 500) },
    { name: 'the probe answers a shape with no items', reply: () => json({}) },
  ])('keeps the authorization button when $name', async ({ reply }) => {
    if (reply) stubAPI(call => call.url === '/api/v1/oauth/clients' ? reply() : null)
    else vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Outlook')
    await waitFor(() => expect(webAuth()).toBeEnabled())
    expect(screen.getByText(/授权窗口/)).toBeInTheDocument()
    expect(screen.queryByLabelText('Outlook Client ID')).not.toBeInTheDocument()
  })

  it('connects from a hand-carried authorization code', async () => {
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [gmailStatus] })
      if (call.url === '/api/v1/oauth/gmail/authorize') {
        return json({
          authorization_url: 'https://accounts.google.com/o/oauth2/v2/auth?client_id=123&state=opaque-state',
          state: 'opaque-state',
          redirect_uri: 'http://localhost:13737/api/v1/oauth/gmail/callback',
        })
      }
      if (call.url === '/api/v1/oauth/gmail/code') return json({ id: 7, email: 'me@gmail.com', provider: 'gmail', status: 'connecting' }, 201)
      return null
    })
    const onCreated = vi.fn()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    pick('Gmail')
    fireEvent.change(screen.getByLabelText('显示名称'), { target: { value: '工作邮箱' } })
    fireEvent.click(screen.getByRole('button', { name: '手动输入授权码' }))
    fireEvent.click(screen.getByRole('button', { name: '获取授权链接' }))

    // The URL is offered as a link into a new tab, because the point of this path is
    // that the consent screen is finished somewhere this window cannot observe.
    const link = await screen.findByRole('link')
    expect(link).toHaveAttribute('href', 'https://accounts.google.com/o/oauth2/v2/auth?client_id=123&state=opaque-state')
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', 'noreferrer')
    // The display name typed above rides along, or the account would come back
    // unnamed on a path the user chose deliberately.
    expect(calls.find(call => call.url === '/api/v1/oauth/gmail/authorize')?.body).toEqual({ display_name: '工作邮箱' })

    // The whole callback address is accepted: that is what the address bar holds.
    fireEvent.change(screen.getByLabelText('授权码或回跳地址'), { target: { value: '  http://localhost:13737/api/v1/oauth/gmail/callback?code=4/0Ax4Xk&state=opaque-state  ' } })
    fireEvent.click(screen.getByRole('button', { name: '完成连接' }))

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(calls.find(call => call.url === '/api/v1/oauth/gmail/code')).toMatchObject({
      method: 'POST',
      body: { state: 'opaque-state', code: 'http://localhost:13737/api/v1/oauth/gmail/callback?code=4/0Ax4Xk&state=opaque-state' },
    })
  })

  it('asks for a fresh link when the state has been spent', async () => {
    let attempt = 0
    stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [gmailStatus] })
      if (call.url === '/api/v1/oauth/gmail/authorize') {
        attempt += 1
        return json({ authorization_url: `https://accounts.google.com/x?state=state-${attempt}`, state: `state-${attempt}`, redirect_uri: 'http://localhost:13737/cb' })
      }
      if (call.url === '/api/v1/oauth/gmail/code') return json({ error: { code: 'invalid_request', message: 'authorization state expired' } }, 400)
      return null
    })
    const onCreated = vi.fn()
    render(<AccountDialog onClose={() => undefined} onCreated={onCreated} />)

    pick('Gmail')
    fireEvent.click(screen.getByRole('button', { name: '手动输入授权码' }))
    fireEvent.click(screen.getByRole('button', { name: '获取授权链接' }))
    await screen.findByRole('link')
    fireEvent.change(screen.getByLabelText('授权码或回跳地址'), { target: { value: 'stale-code' } })
    fireEvent.click(screen.getByRole('button', { name: '完成连接' }))

    // The state is single-use, so a refusal has spent it: retyping into it can never
    // succeed, and the dialog says which action does.
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('authorization state expired')
    expect(alert).toHaveTextContent('授权链接已失效，请重新获取')
    expect(onCreated).not.toHaveBeenCalled()
    expect(screen.queryByRole('link')).not.toBeInTheDocument()

    // And a new one is one press away.
    fireEvent.click(screen.getByRole('button', { name: '获取授权链接' }))
    await waitFor(() => expect(screen.getByRole('link')).toHaveAttribute('href', 'https://accounts.google.com/x?state=state-2'))
  })

  it('reports a refused authorization request instead of an empty panel', async () => {
    stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [gmailStatus] })
      if (call.url === '/api/v1/oauth/gmail/authorize') return json({ error: { code: 'oauth_not_configured', message: 'missing Google OAuth client credentials' } }, 400)
      return null
    })
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Gmail')
    fireEvent.click(screen.getByRole('button', { name: '手动输入授权码' }))
    fireEvent.click(screen.getByRole('button', { name: '获取授权链接' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('missing Google OAuth client credentials')
    expect(screen.getByRole('button', { name: '获取授权链接' })).toBeEnabled()
  })

  // The original report, in the state that produced it: the deployment had never set
  // NEXUSMAIL_MICROSOFT_CLIENT_ID, and the only thing the user saw was the server's
  // English sentence. The server has its own code for that case, so an answer of
  // oauth_not_configured re-reads the status and offers the form that fixes it —
  // whether or not the mount-time probe knew.
  it('offers the client form when the server refuses for want of a client', async () => {
    let known = false
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') {
        // Configured at mount, so the button was there to be pressed; the truth only
        // arrives with the refusal.
        const items = [known ? outlookMissing : { ...outlookMissing, configured: true, source: 'environment', client_id: 'stale' }]
        known = true
        return json({ items })
      }
      if (call.url === '/api/v1/accounts') return json({ error: { code: 'oauth_not_configured', message: 'missing Microsoft OAuth client credentials' } }, 400)
      return null
    })
    vi.stubGlobal('open', vi.fn(() => ({ location: { href: '' }, closed: false, close: vi.fn() })))
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)

    pick('Outlook')
    fireEvent.click(await screen.findByRole('button', { name: '使用网页授权' }))

    // The server's sentence is still shown — it is the evidence — but the remedy is
    // now on screen instead of in a file the user has to go find.
    expect(await screen.findByLabelText('Outlook Client ID')).toBeInTheDocument()
    expect(screen.getByText('missing Microsoft OAuth client credentials')).toBeInTheDocument()
    expect(screen.getByText('NEXUSMAIL_MICROSOFT_CLIENT_ID')).toBeInTheDocument()
    expect(webAuth()).not.toBeInTheDocument()
    expect(calls.filter(call => call.url === '/api/v1/oauth/clients')).toHaveLength(2)
  })

  it('offers no manual channel on the password providers', async () => {
    stubAPI(call => call.url === '/api/v1/oauth/clients' ? json({ items: [gmailStatus, outlookMissing] }) : null)
    render(<AccountDialog onClose={() => undefined} onCreated={() => undefined} />)
    await screen.findByRole('button', { name: /^Outlook需先配置/ })

    // QQ has no authorization code to carry and no client to configure: its 授权码 is
    // issued by the mailbox itself.
    pick('QQ')
    expect(screen.queryByRole('button', { name: '手动输入授权码' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/Client ID/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '继续' })).toBeEnabled()
  })
})

describe('oauth clients in settings', () => {
  afterEach(cleanup)

  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  function open() {
    render(<SettingsDialog preferences={defaultPreferences} accounts={[]} onChange={() => undefined}
      onClose={() => undefined} onAddAccount={() => undefined} onDeleted={() => undefined} onLogout={() => undefined} />)
  }

  it('lists every OAuth provider with where its credentials came from', async () => {
    const stored = { ...outlookMissing, configured: true, source: 'database' as const, client_id: 'stored-id', updated_at: Date.UTC(2026, 0, 2, 3, 4) }
    stubAPI(call => call.url === '/api/v1/oauth/clients' ? json({ items: [gmailStatus, stored] }) : null)
    open()

    expect(await screen.findByText('已配置 · 环境变量')).toBeInTheDocument()
    expect(screen.getByText('已配置 · 页面')).toBeInTheDocument()
    expect(screen.getByLabelText('Gmail Client ID')).toHaveValue('123.apps.googleusercontent.com')
    expect(screen.getByLabelText('Outlook Client ID')).toHaveValue('stored-id')
    // Only the stored row can be handed back to the environment.
    expect(screen.getAllByRole('button', { name: '改回环境变量' })).toHaveLength(1)
  })

  it('shows the unconfigured provider as such', async () => {
    stubAPI(call => call.url === '/api/v1/oauth/clients' ? json({ items: [gmailStatus, outlookMissing] }) : null)
    open()

    expect(await screen.findByText('未配置')).toBeInTheDocument()
    expect(screen.getByText('已配置 · 环境变量')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '改回环境变量' })).not.toBeInTheDocument()
  })

  it('stores a client typed into the panel and flips the badge to the page', async () => {
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [outlookMissing] })
      if (call.url === '/api/v1/oauth/clients/outlook' && call.method === 'PUT') {
        return json({ ...outlookMissing, configured: true, source: 'database', client_id: 'typed-id', updated_at: Date.UTC(2026, 0, 2, 3, 4) })
      }
      return null
    })
    open()

    fireEvent.change(await screen.findByLabelText('Outlook Client ID'), { target: { value: '  typed-id  ' } })
    fireEvent.change(screen.getByLabelText('Outlook Client Secret'), { target: { value: ' typed-secret ' } })
    fireEvent.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByText('已配置 · 页面')).toBeInTheDocument()
    // Surrounding whitespace is what a copy out of a provider console carries, and
    // the server compares the id it stores against what the provider sends back.
    expect(calls.find(call => call.method === 'PUT')?.body).toEqual({ client_id: 'typed-id', client_secret: 'typed-secret' })
    // The stored row can now be handed back to the environment, and the secret is
    // not left sitting in a field after it has been sent.
    expect(screen.getByRole('button', { name: '改回环境变量' })).toBeEnabled()
    expect(screen.getByLabelText('Outlook Client Secret')).toHaveValue('')
    expect(screen.getByLabelText('Outlook Client ID')).toHaveValue('typed-id')
  })

  it('hands a stored client back to the environment and re-reads what stands behind it', async () => {
    const stored = { ...gmailStatus, source: 'database' as const, client_id: 'stored-id' }
    let cleared = false
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/oauth/clients') return json({ items: [cleared ? gmailStatus : stored] })
      if (call.url === '/api/v1/oauth/clients/gmail' && call.method === 'DELETE') {
        cleared = true
        return new Response(null, { status: 204 })
      }
      return null
    })
    open()

    fireEvent.click(await screen.findByRole('button', { name: '改回环境变量' }))

    // The server answers 204, so what remains — an environment variable, or nothing
    // — is only knowable by asking again. What comes back is the environment's own
    // client id, which is not the one that was just cleared.
    expect(await screen.findByDisplayValue('123.apps.googleusercontent.com')).toBeInTheDocument()
    expect(screen.getByText('已配置 · 环境变量')).toBeInTheDocument()
    expect(calls.filter(call => call.url === '/api/v1/oauth/clients')).toHaveLength(2)
    expect(screen.queryByRole('button', { name: '改回环境变量' })).not.toBeInTheDocument()
  })

  it('degrades to one line when the list cannot be read, without alarming the panel', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    open()

    // An older gateway has no such route, and this is not what the user opened
    // settings for: no alert, and the environment path still named.
    expect(await screen.findByText(/暂时读不到 OAuth 客户端配置/)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/Client ID/)).not.toBeInTheDocument()
    // The rest of the panel is untouched.
    expect(screen.getByRole('switch', { name: '自动加载远程图片' })).toBeInTheDocument()
  })
})
