import { expect, test, type Page } from '@playwright/test'

// The OAuth handshake is the only flow in the app that leaves the page and comes
// back, and unit tests can only stub `window.open`. What they cannot show is the
// half that matters here: a real second window loads the same bundle on the
// callback's redirect target, hands the outcome to the window that opened it, and
// closes itself — with the mailbox behind it still mounted.
//
// The consent screen itself is stood in for by the SPA's own callback URL: driving
// a real Google or Microsoft login is not automatable, and the part under test
// starts where the provider's redirect lands.

const account = { id: 1, email: 'me@gmail.com', display_name: 'Gmail', provider: 'gmail', status: 'connecting' }

async function stubAPI(page: Page, outcome: string, accounts: () => unknown[]) {
  const posted: Record<string, unknown>[] = []
  await page.routeWebSocket('**/api/v1/ws', () => undefined)
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (url.pathname === '/api/v1/auth/session') return json({ csrf_token: 'e2e-csrf', expires_at: Date.now() + 60_000 }, 201)
    if (url.pathname === '/api/v1/accounts' && request.method() === 'POST') {
      posted.push(request.postDataJSON())
      // The provider's consent screen is skipped: the popup is pointed at the
      // callback the real redirect would have landed on.
      return json({ authorization_url: `${url.origin}/?oauth=${outcome}` }, 202)
    }
    if (url.pathname === '/api/v1/accounts') return json({ items: accounts() })
    return json({ items: [] })
  })
  return posted
}

async function openDialog(page: Page) {
  await page.goto('/')
  await page.getByLabel('API Key').fill('e2e-api-key-123456789012345678901')
  await page.getByRole('button', { name: '进入 NexusMail' }).click()
  await expect(page.getByRole('heading', { name: 'All Inboxes' })).toBeVisible()
  await page.getByRole('button', { name: '连接邮箱' }).click()
  await expect(page.getByText('选择邮箱服务商')).toBeVisible()
}

test('completes an OAuth connection in a popup and keeps the mailbox mounted', async ({ page }) => {
  let accounts: unknown[] = []
  const posted = await stubAPI(page, 'success', () => accounts)
  await openDialog(page)

  await page.getByRole('button', { name: /^Gmail\b/ }).click()
  // No credential field on this path — the whole point of the OAuth branch.
  await expect(page.getByLabel('授权码')).toHaveCount(0)
  await page.getByLabel('显示名称').fill('个人 Gmail')

  accounts = [account]
  const popup = page.waitForEvent('popup')
  await page.getByRole('button', { name: '使用网页授权' }).click()

  // The popup relays the outcome and closes itself; nothing is left for the user
  // to dismiss.
  await (await popup).waitForEvent('close', { timeout: 15_000 })

  expect(posted).toEqual([{ provider: 'gmail', display_name: '个人 Gmail', auth: { type: 'oauth2' } }])
  // The dialog closed on success and the account list refreshed. Both are only
  // observable because the mailbox behind the popup was never navigated away —
  // which is exactly what the old full-page redirect destroyed.
  await expect(page.getByText('选择邮箱服务商')).toHaveCount(0)
  await expect(page.getByText('me@gmail.com')).toBeVisible()
})

test('reports the reason when authorization fails and allows a retry', async ({ page }) => {
  const posted = await stubAPI(page, 'error&reason=access_denied', () => [])
  await openDialog(page)

  // Hotmail is a chip of its own but authorizes as Outlook, which is the only
  // provider value the server accepts for it.
  await page.getByRole('button', { name: /^Hotmail\b/ }).click()

  const popup = page.waitForEvent('popup')
  await page.getByRole('button', { name: '使用网页授权' }).click()
  await (await popup).waitForEvent('close', { timeout: 15_000 })

  expect(posted).toEqual([{ provider: 'outlook', display_name: '', auth: { type: 'oauth2' } }])
  await expect(page.getByRole('alert')).toContainText('access_denied')
  // The dialog stays usable rather than parking on a spinner.
  await expect(page.getByRole('button', { name: '使用网页授权' })).toBeEnabled()
})
