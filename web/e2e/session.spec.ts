import { expect, test, type BrowserContext, type Page } from '@playwright/test'

const apiKey = 'e2e-session-key-123456789012345678901'
const message = {
  id: 7, account_id: 1, direction: 'incoming', subject: 'Session mail', sender: 'sender@example.com',
  recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: 'Session test',
  body_state: 'ready', body_text: 'Session test', received_at: 0,
  is_read: false, is_starred: false, has_attachments: false,
}

// Exercise browser cookie/storage lifetimes, not just a mock that accepts every
// mutation. The Go regression test pins the matching server-side checks.
async function sessionAPI(context: BrowserContext, options: { cookieSeconds?: number } = {}) {
  const sessions = new Map<string, string>()
  const writes: number[] = []
  let logins = 0
  let read = false
  let logoutFails = false
  let expireOnMutation = false
  await context.routeWebSocket('**/api/v1/ws', () => undefined)
  await context.route('**/api/v1/**', async route => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const method = request.method()
    const headers = await request.allHeaders()
    const json = (body: unknown, status = 200, extraHeaders = {}) =>
      route.fulfill({ status, contentType: 'application/json', headers: extraHeaders, body: JSON.stringify(body) })
    if (path === '/api/v1/auth/session' && method === 'POST') {
      expect(request.postDataJSON()).toEqual({ api_key: apiKey })
      const token = `e2e-session-${++logins}`
      const csrf = `e2e-csrf-${logins}`
      sessions.set(token, csrf)
      const seconds = options.cookieSeconds ?? 3600
      return json({ csrf_token: csrf, expires_at: Date.now() + seconds * 1000 }, 201, {
        'Set-Cookie': `nexusmail_session=${token}; Path=/; HttpOnly; SameSite=Strict; Max-Age=${seconds}`,
      })
    }
    if (path === '/api/v1/auth/session' && method === 'DELETE' && logoutFails) return route.abort('failed')
    const token = /(?:^|;\s*)nexusmail_session=([^;]+)/.exec(headers.cookie ?? '')?.[1] ?? ''
    const mutation = !['GET', 'HEAD', 'OPTIONS'].includes(method)
    // Let the socket's initial feed refresh finish before expiring this write.
    if (expireOnMutation && mutation) {
      sessions.clear()
      expireOnMutation = false
    }
    const csrf = sessions.get(token)
    if (!csrf || (mutation && headers['x-csrf-token'] !== csrf)) {
      if (mutation) writes.push(401)
      // The real middleware tells a missing cookie apart from a bad CSRF token, and
      // the missing-cookie wording is the one that reached the user's toast.
      const reason = token ? 'invalid session or CSRF token' : 'authentication required'
      return json({ error: { code: 'unauthorized', message: reason } }, 401)
    }
    if (path === '/api/v1/auth/session' && method === 'DELETE') {
      sessions.delete(token)
      return route.fulfill({ status: 204, headers: { 'Set-Cookie': 'nexusmail_session=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0' } })
    }
    if (path === '/api/v1/messages/7' && method === 'PATCH') {
      writes.push(200)
      read = true
      return json({ ...message, is_read: true })
    }
    if (path === '/api/v1/messages/mark-read' && method === 'POST') {
      writes.push(200)
      const updated = read ? 0 : 1
      read = true
      return json({ updated })
    }
    if (path === '/api/v1/messages') return json({ items: [{ ...message, is_read: read }], unread_total: read ? 0 : 1 })
    if (path === '/api/v1/messages/7') return json({ message: { ...message, is_read: read }, attachments: [] })
    return json({ items: [] })
  })
  return { writes, expireOnNextMutation: () => { expireOnMutation = true }, failLogout: () => { logoutFails = true }, logins: () => logins }
}

async function signIn(page: Page) {
  await page.getByLabel('API Key').fill(apiKey)
  await page.getByRole('button', { name: '进入 NexusMail' }).click()
  await expect(page.getByRole('button', { name: /Session mail/ })).toBeVisible()
}

for (const action of ['single', 'bulk']) {
  test(`keeps ${action} mark-read authenticated after another tab signs in`, async ({ page, context }) => {
    const server = await sessionAPI(context)
    const second = await context.newPage()
    await page.goto('/')
    await second.goto('/')
    await signIn(page)
    await signIn(second)
    expect(server.logins()).toBe(2)

    const cookie = (await context.cookies()).find(item => item.name === 'nexusmail_session')!
    expect(cookie.httpOnly).toBe(true)
    expect(cookie.sameSite).toBe('Strict')
    if (action === 'single') await page.getByRole('button', { name: /Session mail/ }).click()
    else await page.getByRole('button', { name: '全部已读' }).click()

    await expect.poll(() => server.writes).toEqual([200])
    await page.reload()
    await expect(page.getByRole('button', { name: '全部已读' })).toBeDisabled()
    await expect(page.getByLabel('API Key')).toHaveCount(0)
  })
}

for (const width of [1280, 390]) {
  test(`recovers from an expired mark-read session at width ${width}`, async ({ page, context }) => {
    await page.setViewportSize({ width, height: 844 })
    const server = await sessionAPI(context)
    await page.goto('/')
    await signIn(page)
    server.expireOnNextMutation()

    await page.getByRole('button', { name: /Session mail/ }).click()

    await expect(page.getByLabel('API Key')).toBeVisible()
    expect(server.writes).toEqual([401])
    await page.reload()
    await expect(page.getByLabel('API Key')).toBeVisible()
    await signIn(page)
    await page.getByRole('button', { name: /Session mail/ }).click()
    await expect.poll(() => server.writes).toEqual([401, 200])
    await page.reload()
    await expect(page.getByRole('button', { name: '全部已读' })).toBeDisabled()
  })
}

for (const failure of [false, true]) {
  test(`ends the local session in every tab when logout ${failure ? 'fails' : 'succeeds'}`, async ({ page, context }) => {
    const server = await sessionAPI(context)
    await page.goto('/')
    await signIn(page)
    const second = await context.newPage()
    await second.goto('/')
    await expect(second.getByRole('button', { name: /Session mail/ })).toBeVisible()
    expect(server.logins()).toBe(1)
    if (failure) server.failLogout()

    await second.getByRole('button', { name: '退出', exact: true }).click()

    await expect(second.getByLabel('API Key')).toBeVisible()
    await expect(page.getByLabel('API Key')).toBeVisible()
    await page.reload()
    await expect(page.getByLabel('API Key')).toBeVisible()
  })
}

// The reported defect, with a real browser cookie doing the expiring. The session
// cookie carries a fixed Expires and is never renewed, so it lapses 12 hours after
// login - while the tab stays open and the CSRF token in localStorage stays readable
// forever. Opening a message then fired a PATCH the browser sent with no cookie, and
// the server's `authentication required` was announced as "标记已读失败：…": the wrong
// action blamed, in the wrong language, for a session the user cannot do anything
// about. `isAuthenticated()` only runs at mount, so nothing re-checks it here.
test('does not blame mark-read when the cookie lapses under an open mailbox', async ({ page, context }) => {
  const server = await sessionAPI(context, { cookieSeconds: 3 })
  await page.goto('/')
  await signIn(page)
  // The token outlives the cookie, which is what leaves the mailbox mounted.
  expect(await page.evaluate(() => localStorage.getItem('nexusmail.csrf'))).toBeTruthy()
  await expect.poll(async () => (await context.cookies()).some(item => item.name === 'nexusmail_session'), { timeout: 15_000 }).toBe(false)
  await expect(page.getByRole('button', { name: /Session mail/ })).toBeVisible()

  await page.getByRole('button', { name: /Session mail/ }).click()

  // The write went out cookieless and was refused, so the row rolls back and the
  // session ends - but the failure is never dressed up as a mark-read error.
  await expect.poll(() => server.writes).toEqual([401])
  await expect(page.getByText(/标记已读失败/)).toBeHidden()
  await expect(page.getByText(/authentication required/)).toBeHidden()
  await expect(page.getByLabel('API Key')).toBeVisible()
  await signIn(page)
})
