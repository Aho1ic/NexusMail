import { expect, test, type Page } from '@playwright/test'

const account = { id: 1, email: 'mail@example.com', display_name: '工作邮箱', provider: 'qq', status: 'connected' }

function message(subject: string, isRead: boolean, bodyHTML = '') {
  return {
    id: 7, account_id: 1, direction: 'incoming', subject, sender: 'Sender <sender@example.com>',
    recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: '实时邮件流测试',
    body_state: bodyHTML ? 'ready' : 'metadata', body_html: bodyHTML || undefined, received_at: Date.now(),
    is_read: isRead, is_starred: false, has_attachments: false,
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
    if (url.pathname === '/api/v1/messages/7') return json({ message: feed()[0], attachments: [] })
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

// The reported bug in its original shape: the badge counts an unread message that
// the first page never loaded, so the list could not show it and the count never
// cleared. 68 read rows put the unread one at row 69, matching the live mailbox.
test('jumps to unread mail sitting below the first page', async ({ page }) => {
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  const items = Array.from({ length: 68 }, (_, index) => ({ ...message(`已读 ${index + 1}`, true), id: index + 1, received_at: Date.now() - index }))
  items.push({ ...message('深页未读', false), id: 69, received_at: Date.now() - 68 })
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts') return json({ items: [account] })
    if (url.pathname === '/api/v1/messages') {
      const wide = url.searchParams.get('limit') === '100'
      return json({ items: wide ? items : items.slice(0, 40), unread_total: 1, next_cursor: wide ? undefined : 'cursor-1' })
    }
    return json({ items: [] })
  })

  await login(page)
  const row = page.getByRole('button', { name: /深页未读/ })
  await expect(row).toHaveCount(0)

  await page.getByRole('button', { name: /All Inboxes/ }).click()

  await expect(row).toBeInViewport()
  // Taken to the mail, not into it: still unread, still counted, no mark-read noise.
  await expect(row).toHaveAttribute('data-revealed', '')
  await expect(page.getByRole('button', { name: /All Inboxes/ })).toContainText('1')
  await expect(page.getByRole('status')).toHaveCount(0)
})

// The body frame cannot measure itself — it is a scriptless sandbox — so it is given
// the pane's leftover height instead. It used to be a fixed 520px band, which on a
// large screen left the lower half of the reading pane blank while the message
// scrolled inside that band. Asserted as a share of the pane rather than in pixels,
// since the header above the frame is what the rest of the height goes to.
const bodyHTML = `<div style="width:600px">${'<p>正文段落。</p>'.repeat(60)}</div>`

function paneBox(page: Page) {
  return page.evaluate(() => {
    const article = document.querySelector('article')!
    const element = document.querySelector('iframe[title="邮件正文"]')!
    return { pane: article.clientHeight, frame: element.clientHeight, overflow: article.scrollHeight - article.clientHeight }
  })
}

async function openBody(page: Page, width: number, height: number) {
  await page.setViewportSize({ width, height })
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await stubAPI(page, () => [message('正文高度测试', true, bodyHTML)])
  await login(page)
  await page.getByRole('button', { name: /正文高度测试/ }).first().click()
  await expect(page.getByTitle('邮件正文')).toBeVisible()
}

test('grows the body frame to fill the reading pane on a large screen', async ({ page }) => {
  await openBody(page, 2560, 1400)

  const box = await paneBox(page)

  // Well past the old 520px, and the pane itself does not scroll: the frame took the
  // room instead of leaving it empty.
  expect(box.frame).toBeGreaterThan(box.pane * 0.75)
  expect(box.overflow).toBe(0)
})

// A 1440x900 pane has ~469px left after its header, so the 520px floor was taller
// than the space available and pushed the article into an outer scrollbar to honour a
// minimum the screen could have filled on its own. Above md the floor is 200px, so
// flex decides and the pane stays exactly one screen.
test('does not force the pane to scroll on a laptop screen', async ({ page }) => {
  await openBody(page, 1440, 900)

  const box = await paneBox(page)

  expect(box.overflow).toBe(0)
  expect(box.frame).toBeGreaterThan(box.pane * 0.5)
})
