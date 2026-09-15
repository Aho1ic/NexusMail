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
// observed without a real gateway. accounts is overridable for the title tests, which
// need an account whose title is a single long run.
async function stubAPI(page: Page, feed: () => unknown[], onMarkRead: (url: string) => unknown = () => ({ updated: 1 }), accounts = [account]) {
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts') return json({ items: accounts })
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
  // The account row is one line: the title only, no address sublabel. exact
  // because every row's accessible name ends with the account chip's title too.
  await expect(page.getByRole('button', { name: '工作邮箱', exact: true })).toBeVisible()
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

// The reading column used to be capped at 1600px, which only ever binds on a display
// wide enough to give the pane more than that: at 2560 — a 27-inch screen at its
// native logical size — the nav and the list column leave the pane 1802px, and mail
// does not come in that width. A newsletter is 750-860px, and mail whose fixed width
// sits nested inside a full-width wrapper is pinned to the left of the frame, so the
// message read as a strip down the left with most of the pane blank to its right.
// Capping the column at a reading measure and centring it splits that leftover width
// between two equal margins instead, and the subject and the frame end on one edge.
const newsletter = (width: number) => `<table width="100%" cellpadding="0" cellspacing="0"><tr><td><table width="${width}" style="width:${width}px"><tr><td style="padding:0 24px"><p style="margin:0;font-size:14px;line-height:24px">${'正文段落。'.repeat(30)}</p></td></tr></table></td></tr></table>`

function columnBox(page: Page) {
  return page.evaluate(() => {
    const pane = document.querySelector('main')!
    const article = pane.querySelector('article')!
    const column = article.firstElementChild as HTMLElement
    const heading = article.querySelector('h1')!
    const frame = article.querySelector('iframe')!
    const paneRect = pane.getBoundingClientRect()
    const columnRect = column.getBoundingClientRect()
    return {
      width: Math.round(columnRect.width),
      left: Math.round(columnRect.left - paneRect.left),
      right: Math.round(paneRect.right - columnRect.right),
      // Negative means the box ends short of the column it sits in, which is what
      // left the subject stranded in the middle of a 1600px row.
      subjectPastColumn: Math.round(heading.getBoundingClientRect().right - columnRect.right),
      framePastColumn: Math.round(frame.getBoundingClientRect().right - columnRect.right),
      paneOverflow: article.scrollWidth - article.clientWidth,
    }
  })
}

async function openNewsletter(page: Page, width: number, subject: string) {
  await page.setViewportSize({ width: 2560, height: 1385 })
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await stubAPI(page, () => [message(subject, true, newsletter(width))])
  await login(page)
  await page.getByRole('button', { name: new RegExp(subject) }).first().click()
  await expect(page.getByTitle('邮件正文')).toBeVisible()
}

test('keeps the message column a reading measure on a 27-inch display', async ({ page }) => {
  await openNewsletter(page, 800, '宽屏留白测试')

  const box = await columnBox(page)

  // The mail is 800px wide, so the column is not 1600 just because the pane is: the
  // cap, not the pane, decides how wide the message reads.
  expect(box.width).toBeLessThanOrEqual(1024)
  // Centred: the width the pane has over is split, not heaped on the right.
  expect(Math.abs(box.left - box.right), `margins ${box.left} / ${box.right}`).toBeLessThanOrEqual(1)
  // One right edge for the whole message — subject, sender row and body frame.
  expect(box.subjectPastColumn, 'the subject stops short of the column').toBeGreaterThanOrEqual(-1)
  expect(box.framePastColumn).toBe(0)
  expect(box.paneOverflow).toBe(0)
})

// The cap has to stay above what real mail is built for, and the frame keeps the full
// column. A design wider than that must scroll inside its own box — the one the
// wrapper below provides — rather than being squeezed to the column or dragging the
// pane sideways with it.
test('keeps mail wider than the column scrolling in place', async ({ page }) => {
  await openNewsletter(page, 1300, '超宽邮件测试')

  const box = await columnBox(page)
  const frame = page.frames().find(candidate => candidate !== page.mainFrame())!
  const inner = await frame.evaluate(() => {
    const wrapper = document.querySelector('.nexusmail-scroll')!
    return { scroll: wrapper.scrollWidth - wrapper.clientWidth, content: wrapper.scrollWidth }
  })

  expect(box.paneOverflow).toBe(0)
  expect(inner.content, 'the 1300px design was squeezed to the column').toBeGreaterThanOrEqual(1300)
  expect(inner.scroll, 'the wide design has nowhere to scroll').toBeGreaterThan(0)
})

// The pane title shares its row with the hamburger and the toolbar, so it is a flex
// item beside them. A flex item will not shrink below its longest unbreakable run
// unless something says otherwise, and an account with no display name falls back to
// its address for the title — one long run. The row therefore grew past the pane at
// every width below 2xl and the pane's own overflow-hidden clipped the end of the
// title together with both toolbar buttons: 132px of the row outside a 320px pane,
// 77px at 375 and 40px at 1024, with the refresh button up to 131px beyond the edge.
// The layout half of this cannot be asserted in jsdom, which has no layout engine.
const longAddress = 'avery.long.account.name@sub-company-example.com'
const widths = [320, 375, 768, 1024, 1280, 1536]

function titleBox(page: Page) {
  return page.evaluate(() => {
    const pane = document.querySelector('section')!
    const heading = pane.querySelector('header h1')!
    const buttons = pane.querySelectorAll('header button')
    const refresh = buttons[buttons.length - 1]
    const paneRect = pane.getBoundingClientRect()
    const lineHeight = parseFloat(getComputedStyle(heading).lineHeight)
    return {
      paneOverflow: pane.scrollWidth - pane.clientWidth,
      titlePastPane: Math.round(heading.getBoundingClientRect().right - paneRect.right),
      refreshPastPane: Math.round(refresh.getBoundingClientRect().right - paneRect.right),
      titleOverflow: heading.scrollWidth - heading.clientWidth,
      titleLines: Math.round(heading.getBoundingClientRect().height / lineHeight),
    }
  })
}

test('keeps a long pane title inside the pane at every width', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 900 })
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await stubAPI(page, () => [message('标题测试', true)], undefined, [{ ...account, display_name: '', email: longAddress }])
  await login(page)

  // Below lg the nav is an overlay, so the account row is only reachable once it is
  // open; at lg and up the nav holds a column and is always there. The view survives a
  // viewport change, so the title is chosen once and then measured at every width.
  await page.getByRole('button', { name: '打开文件夹' }).click()
  await page.getByRole('complementary').getByRole('button', { name: longAddress }).click()

  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 })
    const box = await titleBox(page)

    expect(box.paneOverflow, `the pane scrolls sideways at ${width}px`).toBe(0)
    expect(box.titlePastPane, `the title leaves the pane at ${width}px`).toBeLessThanOrEqual(0)
    expect(box.refreshPastPane, `the toolbar leaves the pane at ${width}px`).toBeLessThanOrEqual(0)
    // Wrapped, not clipped: the title's own box has to contain its text.
    expect(box.titleOverflow, `the title is clipped at ${width}px`).toBeLessThanOrEqual(0)
    expect(box.titleLines, `the title vanished at ${width}px`).toBeGreaterThanOrEqual(1)
  }
})

// A bracketed order number is one unbreakable run wider than the reading column at
// every width below xl (333px of text in a 268px column at lg). The reading pane has
// no scrollbar of its own, so without overflow-wrap the run is clipped silently rather
// than wrapped — nothing overflows visibly, the subject just loses its right-hand end.
const unbreakableSubject = '您的京东订单【3619484002602023】电子发票已开具，请查收附件并妥善保存'

test('keeps an unbreakable reading-pane subject inside its column', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 900 })
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await stubAPI(page, () => [message(unbreakableSubject, true)])
  await login(page)
  await page.getByRole('button', { name: /京东订单/ }).first().click()

  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 })
    const box = await page.evaluate(() => {
      const article = document.querySelector('article')!
      const heading = article.querySelector('h1')!
      return {
        articleOverflow: article.scrollWidth - article.clientWidth,
        subjectOverflow: heading.scrollWidth - heading.clientWidth,
        lines: Math.round(heading.getBoundingClientRect().height / parseFloat(getComputedStyle(heading).lineHeight)),
      }
    })

    expect(box.articleOverflow, `the reading pane scrolls sideways at ${width}px`).toBe(0)
    expect(box.subjectOverflow, `the subject overflows its column at ${width}px`).toBeLessThanOrEqual(0)
    // The run is longer than one line at every one of these widths, so a single line
    // means the guard dropped out and the text ran off instead of wrapping.
    expect(box.lines, `the subject did not wrap at ${width}px`).toBeGreaterThan(1)
  }
})
