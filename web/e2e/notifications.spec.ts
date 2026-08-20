import { expect, test, type Page, type WebSocketRoute } from '@playwright/test'

// The default headless shell reports Notification.permission as 'denied' and cannot
// be granted it, so this file runs on full Chromium. Everything here depends on the
// permission being real: 127.0.0.1 is already a secure context, which is what makes
// the service worker and the clipboard available.
test.use({ channel: 'chromium' })

const origin = 'http://127.0.0.1:4173'

const account = { id: 1, email: 'mail@example.com', display_name: '工作邮箱', provider: 'qq', status: 'connected' }

const mail = {
  id: 7, account_id: 1, direction: 'incoming', subject: '【示例服务】验证码', sender: 'Robot <robot@example.com>',
  recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: '您的验证码是 482913',
  body_state: 'ready', body_text: '您的验证码是 482913，10 分钟内有效。', received_at: Date.now(),
  is_read: true, is_starred: false, has_attachments: false,
}

async function stubAPI(page: Page) {
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts') return json({ items: [account] })
    if (url.pathname === '/api/v1/messages/7') return json({ message: mail, attachments: [], otp_code: '482913' })
    if (url.pathname === '/api/v1/messages') return json({ items: [mail] })
    return json({ items: [] })
  })
}

async function login(page: Page) {
  await page.goto('/')
  await page.getByLabel('API Key').fill('e2e-api-key-123456789012345678901')
  await page.getByRole('button', { name: '进入 NexusMail' }).click()
  await expect(page.getByRole('heading', { name: 'All Inboxes' })).toBeVisible()
}

test('raises a copyable notification for a verification code pushed over the socket', async ({ context, page }) => {
  await context.grantPermissions(['notifications'], { origin })
  // The OS cannot be asked what it is displaying, so the call the app makes is
  // recorded and deliberately not forwarded — a real notification would linger in
  // the developer's notification centre after every run. Everything up to the call
  // is real, including the service worker that makes the copy button possible.
  await page.addInitScript(() => {
    const seen: unknown[] = []
    Object.defineProperty(window, '__notices', { value: seen })
    ServiceWorkerRegistration.prototype.showNotification = async (title: string, options?: NotificationOptions) => { seen.push({ title, options }) }
  })

  let socket: WebSocketRoute | undefined
  await page.routeWebSocket('**/api/v1/ws', route => { socket = route })
  await stubAPI(page)

  await login(page)
  // showOTPNotification needs the registration, so the frame must not race it.
  await page.waitForFunction(async () => Boolean(await navigator.serviceWorker.getRegistration()))
  await expect.poll(() => Boolean(socket)).toBeTruthy()

  await socket!.send(JSON.stringify({
    type: 'NEW_EMAIL', sequence: 1, occurred_at: Date.now(),
    data: { message_id: 7, account_id: 1, otp_code: '482913', otp_subject: '【示例服务】验证码' },
  }))

  await expect.poll(() => page.evaluate(() => (window as unknown as { __notices: unknown[] }).__notices)).toEqual([{
    title: '收到验证码',
    options: expect.objectContaining({
      body: '482913\n【示例服务】验证码',
      // The tag lets the body pass replace the arrival guess instead of stacking.
      tag: 'otp-7',
      requireInteraction: true,
      actions: [{ action: 'copy', title: '复制验证码' }],
      data: { code: '482913', messageID: 7 },
    }),
  }])
})

test('copies the code to the real clipboard from the detail view', async ({ context, page }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin })
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await stubAPI(page)

  await login(page)
  await page.getByRole('button', { name: /验证码/ }).first().click()

  await page.getByRole('button', { name: '复制验证码 482913' }).click()

  await expect(page.getByRole('status')).toHaveText('已复制验证码 482913')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('482913')
})
