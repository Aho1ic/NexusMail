import { expect, test, type Page } from '@playwright/test'

const account = { id: 1, email: 'mail@example.com', display_name: '工作邮箱', provider: 'qq', status: 'connected' }

function message(id: number, subject: string, fromEmail: string, isRead = false) {
  return {
    id, account_id: 1, direction: 'incoming', subject, sender: `Sender <${fromEmail}>`,
    recipients: 'mail@example.com', from: JSON.stringify([`Sender <${fromEmail}>`]),
    to: '[]', cc: '[]', bcc: '[]', snippet: 'snippet',
    body_state: 'ready', body_text: `body ${id}`, received_at: Date.now() - id,
    is_read: isRead, is_starred: false, has_attachments: false,
  }
}

async function login(page: Page) {
  await page.goto('/')
  await page.getByLabel('API Key').fill('e2e-api-key-123456789012345678901')
  await page.getByRole('button', { name: '进入 NexusMail' }).click()
  await expect(page.getByRole('heading', { name: 'All Inboxes' })).toBeVisible()
}

async function stub(page: Page, items: unknown[]) {
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts') return json({ items: [account] })
    if (url.pathname.startsWith('/api/v1/messages/') && route.request().method() === 'GET') {
      const id = Number(url.pathname.split('/').pop())
      const found = (items as Array<{ id: number }>).find(item => item.id === id)
      return json({ message: found ?? (items as Array<unknown>)[0], attachments: [] })
    }
    if (url.pathname === '/api/v1/messages') {
      const sender = url.searchParams.get('sender')
      const filtered = sender
        ? (items as Array<{ from: string; sender: string }>).filter(item =>
            item.from.toLowerCase().includes(sender.toLowerCase()) || item.sender.toLowerCase().includes(sender.toLowerCase()))
        : items
      return json({ items: filtered, unread_total: (filtered as Array<{ is_read: boolean }>).filter(item => !item.is_read).length })
    }
    return json({})
  })
}

test('opens a sender stack and then an individual message', async ({ page }) => {
  const items = [
    message(3, 'A-3', 'a@x.com'),
    message(2, 'A-2', 'a@x.com'),
    message(1, 'B-1', 'b@x.com'),
  ]
  await stub(page, items)
  await login(page)

  // Enable basic sender stacking from settings.
  await page.getByRole('button', { name: '设置' }).click()
  await page.getByRole('switch', { name: '相同的发信者进行折叠' }).click()
  await page.getByRole('button', { name: '完成' }).click()

  const stack = page.getByRole('button', { name: 'Sender 的 2 封邮件' })
  await expect(stack).toBeVisible()
  await stack.click()
  await expect(stack).toHaveAttribute('aria-current', 'true')

  await expect(page.getByText('Sender stack')).toBeVisible()
  await expect(page.getByRole('button', { name: /A-2/ })).toBeVisible()
  await page.getByRole('button', { name: /A-3/ }).click()
  await expect(page.getByRole('heading', { name: 'A-3' })).toBeVisible()
})

test('consecutive stacks mark the batch read on open', async ({ page }) => {
  const items = [
    message(3, 'A-3', 'a@x.com'),
    message(2, 'A-2', 'a@x.com'),
  ]
  const patches: string[] = []
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts') return json({ items: [account] })
    if (route.request().method() === 'PATCH') {
      patches.push(url.pathname)
      const id = Number(url.pathname.split('/').pop())
      const found = (items as Array<{ id: number; is_read: boolean }>).find(item => item.id === id)
      if (found) found.is_read = true
      return json(found)
    }
    if (url.pathname.startsWith('/api/v1/messages/') && route.request().method() === 'GET') {
      const id = Number(url.pathname.split('/').pop())
      return json({ message: (items as Array<unknown>).find(item => (item as { id: number }).id === id), attachments: [] })
    }
    if (url.pathname === '/api/v1/messages') {
      return json({ items, unread_total: (items as Array<{ is_read: boolean }>).filter(item => !item.is_read).length })
    }
    return json({})
  })
  await login(page)

  await page.getByRole('button', { name: '设置' }).click()
  await page.getByRole('switch', { name: '进阶：连续同发件人堆叠' }).click()
  await page.getByRole('button', { name: '完成' }).click()

  const stack = page.getByRole('button', { name: 'Sender 的 2 封邮件' })
  await expect(stack).toBeVisible()
  await stack.click()
  await expect(page.getByText('Sender stack')).toBeVisible()
  await expect.poll(() => patches.length).toBeGreaterThanOrEqual(2)
})

test('detail overflow menu lists share and junk actions', async ({ page }) => {
  await stub(page, [message(7, '一封邮件', 'a@x.com', true)])
  await login(page)
  await page.getByRole('button', { name: /一封邮件/ }).click()
  await page.getByRole('button', { name: '更多操作' }).click()
  await expect(page.getByRole('menu')).toBeVisible()
  await expect(page.getByRole('menuitem', { name: '分享' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: '举报垃圾邮件' })).toBeVisible()
})
