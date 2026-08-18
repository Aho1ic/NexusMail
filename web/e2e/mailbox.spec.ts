import { expect, test } from '@playwright/test'

test('authenticates and renders the unified inbox', async ({ page }) => {
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname
    if (path === '/api/v1/auth/session') {
      await route.fulfill({ status: 201, contentType: 'application/json', body: JSON.stringify({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }) })
      return
    }
    if (path === '/api/v1/accounts') {
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [{ id: 1, email: 'mail@example.com', display_name: '工作邮箱', provider: 'qq', status: 'connected' }] }) })
      return
    }
    if (path === '/api/v1/messages') {
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [{ id: 7, account_id: 1, direction: 'incoming', subject: 'NexusMail 已就绪', sender: 'Sender <sender@example.com>', recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: '实时邮件流测试', body_state: 'metadata', received_at: Date.now(), is_read: false, is_starred: false, has_attachments: false }] }) })
      return
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [] }) })
  })

  await page.goto('/')
  await page.getByLabel('API Key').fill('e2e-api-key-123456789012345678901')
  await page.getByRole('button', { name: '进入 NexusMail' }).click()

  await expect(page.getByRole('heading', { name: 'All Inboxes' })).toBeVisible()
  await expect(page.getByText('NexusMail 已就绪')).toBeVisible()
  await expect(page.getByRole('button', { name: '工作邮箱 mail@example.com' })).toBeVisible()
})
