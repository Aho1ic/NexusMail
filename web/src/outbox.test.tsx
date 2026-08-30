import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { OutboxDialog } from './components/OutboxDialog'
import type { Draft } from './types'

// The outbox is where an ambiguous delivery is resolved by hand, so the controls
// it offers per status are a correctness question, not cosmetics: retrying an
// `unknown` draft may deliver a second copy, and retrying something already in
// flight would do it silently. Only the states the server accepts may be offered.

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

function draft(overrides: Partial<Draft> = {}): Draft {
  return {
    id: 1, account_id: 1, revision: 1, to: JSON.stringify(['her@example.com']), cc: '[]', bcc: '[]',
    subject: '季度报告', body_text: '见附件', status: 'draft', remote_sync_state: 'synced',
    updated_at: Date.UTC(2026, 0, 2, 3, 4, 5), ...overrides,
  }
}

type Route = { method: string; url: string }

function stubDrafts(pages: Draft[][], overrides: Record<string, () => Response> = {}) {
  const routes: Route[] = []
  let page = 0
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    routes.push({ method, url })
    const override = overrides[`${method} ${url}`]
    if (override) return override()
    if (url === '/api/v1/drafts' && method === 'GET') {
      const items = pages[Math.min(page, pages.length - 1)]
      page += 1
      return json({ items })
    }
    throw new Error(`unexpected ${method} ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return { routes, fetchMock }
}

function row(subject: string) {
  // Each draft renders as one card; the subject is its only stable anchor.
  const heading = screen.getByText(subject)
  const card = heading.closest('div.rounded-2xl')
  if (!card) throw new Error(`no card around ${subject}`)
  return within(card as HTMLElement)
}

describe('outbox dialog', () => {
  afterEach(cleanup)

  beforeEach(() => {
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  it('shows the empty state only after the load settles', async () => {
    stubDrafts([[]])
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)
    // While loading, "no pending mail" would be a lie.
    expect(screen.queryByText('没有待处理邮件')).not.toBeInTheDocument()
    expect(await screen.findByText('没有待处理邮件')).toBeInTheDocument()
  })

  it('labels every status and flags a remote conflict', async () => {
    const statuses: Array<[string, string]> = [
      ['draft', '草稿'], ['queued', '等待发送'], ['sending', '发送中'],
      ['retry_wait', '等待重试'], ['failed', '失败'], ['unknown', '结果未知'], ['sent', '已发送'],
    ]
    stubDrafts([statuses.map(([status], index) => draft({ id: index + 1, status, subject: `主题 ${status}` }))])
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    await screen.findByText('主题 draft')
    for (const [status, label] of statuses) {
      expect(row(`主题 ${status}`).getByText(label)).toBeInTheDocument()
    }

    cleanup()
    stubDrafts([[draft({ status: 'draft', remote_sync_state: 'conflict' })]])
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)
    expect(await screen.findByText('草稿 · 冲突')).toBeInTheDocument()
  })

  it('offers retry only for the states the server will accept', async () => {
    stubDrafts([[
      draft({ id: 1, status: 'draft', subject: '主题 draft' }),
      draft({ id: 2, status: 'queued', subject: '主题 queued' }),
      draft({ id: 3, status: 'sending', subject: '主题 sending' }),
      draft({ id: 4, status: 'retry_wait', subject: '主题 retry_wait' }),
      draft({ id: 5, status: 'failed', subject: '主题 failed' }),
      draft({ id: 6, status: 'unknown', subject: '主题 unknown' }),
      draft({ id: 7, status: 'sent', subject: '主题 sent' }),
    ]])
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)
    await screen.findByText('主题 draft')

    // Retry is a deliberate act on a delivery that failed or may have half-happened.
    for (const status of ['failed', 'unknown']) {
      expect(row(`主题 ${status}`).getByText('确认重试')).toBeInTheDocument()
    }
    for (const status of ['draft', 'queued', 'sending', 'retry_wait', 'sent']) {
      expect(row(`主题 ${status}`).queryByText('确认重试')).not.toBeInTheDocument()
    }
    // Editing is only meaningful while nothing is in flight.
    for (const status of ['draft', 'failed', 'unknown']) {
      expect(row(`主题 ${status}`).getByText('编辑')).toBeInTheDocument()
    }
    for (const status of ['queued', 'sending', 'retry_wait', 'sent']) {
      expect(row(`主题 ${status}`).queryByText('编辑')).not.toBeInTheDocument()
    }
    // Deleting a draft the worker is holding would race the send.
    for (const status of ['queued', 'sending']) {
      expect(row(`主题 ${status}`).queryByText('删除')).not.toBeInTheDocument()
    }
    for (const status of ['draft', 'retry_wait', 'failed', 'unknown', 'sent']) {
      expect(row(`主题 ${status}`).getByText('删除')).toBeInTheDocument()
    }
  })

  it('retries once and reloads so the new status replaces the old one', async () => {
    const { routes } = stubDrafts(
      [[draft({ id: 7, status: 'failed' })], [draft({ id: 7, status: 'queued' })]],
      { 'POST /api/v1/drafts/7/retry': () => json({}, 202) },
    )
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    fireEvent.click(await screen.findByText('确认重试'))
    expect(await screen.findByText('等待发送')).toBeInTheDocument()
    expect(screen.queryByText('确认重试')).not.toBeInTheDocument()
    expect(routes).toEqual([
      { method: 'GET', url: '/api/v1/drafts' },
      { method: 'POST', url: '/api/v1/drafts/7/retry' },
      { method: 'GET', url: '/api/v1/drafts' },
    ])
  })

  it('deletes and reloads, leaving the empty state', async () => {
    const { routes } = stubDrafts(
      [[draft({ id: 9, status: 'failed' })], []],
      { 'DELETE /api/v1/drafts/9': () => new Response(null, { status: 204 }) },
    )
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    fireEvent.click(await screen.findByText('删除'))
    expect(await screen.findByText('没有待处理邮件')).toBeInTheDocument()
    expect(routes.map(route => `${route.method} ${route.url}`)).toEqual([
      'GET /api/v1/drafts', 'DELETE /api/v1/drafts/9', 'GET /api/v1/drafts',
    ])
  })

  it('hands the whole draft to the editor without touching the server', async () => {
    const target = draft({ id: 4, status: 'failed' })
    const { routes } = stubDrafts([[target]])
    const onEdit = vi.fn()
    render(<OutboxDialog onClose={() => undefined} onEdit={onEdit} />)

    fireEvent.click(await screen.findByText('编辑'))
    // The revision has to travel with it, or the composer's If-Match save fails.
    expect(onEdit).toHaveBeenCalledWith(target)
    expect(routes).toHaveLength(1)
  })

  it('surfaces a failed retry and keeps the row actionable', async () => {
    stubDrafts(
      [[draft({ id: 7, status: 'unknown' })]],
      { 'POST /api/v1/drafts/7/retry': () => json({ error: { code: 'conflict', message: '草稿状态已变化' } }, 409) },
    )
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    fireEvent.click(await screen.findByText('确认重试'))
    expect(await screen.findByText('草稿状态已变化')).toBeInTheDocument()
    expect(screen.getByText('确认重试')).toBeInTheDocument()
  })

  it('surfaces a failed delete without dropping the row from the list', async () => {
    stubDrafts(
      [[draft({ id: 9, status: 'failed', subject: '待删除' })]],
      { 'DELETE /api/v1/drafts/9': () => json({ error: { code: 'internal', message: '删除失败' } }, 500) },
    )
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    fireEvent.click(await screen.findByText('删除'))
    expect(await screen.findByText('删除失败')).toBeInTheDocument()
    expect(screen.getByText('待删除')).toBeInTheDocument()
  })

  it('surfaces a failed load and clears the error once a reload succeeds', async () => {
    let attempt = 0
    const fetchMock = vi.fn(async () => {
      attempt += 1
      return attempt === 1
        ? json({ error: { code: 'internal', message: '读取草稿失败' } }, 500)
        : json({ items: [draft({ subject: '恢复的草稿' })] })
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    expect(await screen.findByText('读取草稿失败')).toBeInTheDocument()
    // The refresh control is the only way out of a failed load.
    fireEvent.click(screen.getByRole('button', { name: '刷新' }))
    expect(await screen.findByText('恢复的草稿')).toBeInTheDocument()
    expect(screen.queryByText('读取草稿失败')).not.toBeInTheDocument()
  })

  it('renders the server-side failure reason on the row that carries it', async () => {
    stubDrafts([[draft({ id: 3, status: 'failed', last_error: '535 authentication failed' })]])
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)
    expect(await screen.findByText('535 authentication failed')).toBeInTheDocument()
  })

  it('falls back to placeholders for an unfinished draft', async () => {
    stubDrafts([[draft({ subject: '', to: '[]' })]])
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)
    expect(await screen.findByText('（无主题）')).toBeInTheDocument()
    expect(screen.getByText(/尚未填写/)).toBeInTheDocument()
  })

  it('closes on request', async () => {
    stubDrafts([[]])
    const onClose = vi.fn()
    render(<OutboxDialog onClose={onClose} onEdit={() => undefined} />)
    await screen.findByText('没有待处理邮件')
    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('sends the CSRF token on the writes and not on the reads', async () => {
    const seen: Array<{ method: string; csrf: string | null }> = []
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = (init?.method ?? 'GET').toUpperCase()
      seen.push({ method, csrf: new Headers(init?.headers).get('X-CSRF-Token') })
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return json({ items: seen.length === 1 ? [draft({ id: 9, status: 'failed' })] : [] })
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<OutboxDialog onClose={() => undefined} onEdit={() => undefined} />)

    fireEvent.click(await screen.findByText('删除'))
    await waitFor(() => expect(seen).toHaveLength(3))
    expect(seen[0]).toEqual({ method: 'GET', csrf: null })
    expect(seen[1]).toEqual({ method: 'DELETE', csrf: 'csrf-value' })
  })
})
