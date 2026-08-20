import { expect, test, type Page } from '@playwright/test'

const account = { id: 1, email: 'mail@example.com', display_name: '工作邮箱', provider: 'qq', status: 'connected' }

function message(subject: string, isRead: boolean) {
  return {
    id: 7, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: '实时邮件流测试',
    body_state: 'metadata', received_at: Date.now(), is_read: isRead, is_starred: false, has_attachments: false,
  }
}

// stubAPI serves the whole app from fixtures. feed() is read per request so a test
// can change what the next reload sees, which is how the mark-read refresh is
// observed without a real gateway.
async function stubAPI(page: Page, feed: () => unknown[], onMarkRead: (url: string) => unknown = () => ({ updated: 1 })) {
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts') return json({ items: [account] })
    if (url.pathname === '/api/v1/messages/mark-read') return json(onMarkRead(url.search))
    if (url.pathname === '/api/v1/messages') return json({ items: feed() })
    return json({ items: [] })
  })
}

async function login(page: Page) {
  await page.goto('/')
  await page.getByLabel('API Key').fill('e2e-api-key-123456789012345678901')
  await page.getByRole('button', { name: '进入 NexusMail' }).click()
  await expect(page.getByRole('heading', { name: 'All Inboxes' })).toBeVisible()
}

test('authenticates and renders the unified inbox', async ({ page }) => {
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await stubAPI(page, () => [message('NexusMail 已就绪', false)])

  await login(page)

  await expect(page.getByText('NexusMail 已就绪')).toBeVisible()
  await expect(page.getByRole('button', { name: '工作邮箱 mail@example.com' })).toBeVisible()
})

test('marks the visible inbox read and reloads it', async ({ page }) => {
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  let read = false
  await stubAPI(page, () => [message('NexusMail 已就绪', read)], search => { read = true; return { updated: 4, search } })

  await login(page)
  const button = page.getByRole('button', { name: '全部已读' })
  await expect(button).toBeEnabled()

  const request = page.waitForRequest(req => req.url().includes('/messages/mark-read') && req.method() === 'POST')
  await button.click()

  expect(new URL((await request).url()).search).toBe('?folder=inbox')
  await expect(page.getByRole('status')).toHaveText('已标记 4 封为已读')
  // The count comes from the reloaded feed, so a stale badge would leave it enabled.
  await expect(button).toBeDisabled()
})
