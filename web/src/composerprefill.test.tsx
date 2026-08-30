import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Composer } from './components/Composer'
import type { Account, Draft, Message } from './types'

// composer.test.tsx pins the one-draft invariant across the autosave chain. This
// covers the rest of the component: what a reply and a reopened draft prefill, the
// attachment path, and every failure that must leave the window usable — a composer
// that swallows an error loses whatever the user typed.

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const accounts: Account[] = [
  { id: 1, email: 'me@qq.com', display_name: '主号', provider: 'qq', status: 'connected' },
  { id: 2, email: 'alt@163.com', display_name: '', provider: '163', status: 'connected' },
]

function draft(overrides: Partial<Draft> = {}): Draft {
  return {
    id: 501, account_id: 1, revision: 3, to: '[]', cc: '[]', bcc: '[]', subject: '', body_text: '',
    status: 'draft', remote_sync_state: 'dirty', updated_at: 0, ...overrides,
  }
}

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: 9, account_id: 1, direction: 'incoming', subject: '发布计划', sender: 'Ada Lovelace <ada@example.com>',
    recipients: '[]', from: '', to: '', cc: '', bcc: '', snippet: '周五之前给我答复', body_state: 'ready',
    received_at: 0, is_read: true, is_starred: false, has_attachments: false, ...overrides,
  }
}

type Call = { url: string; method: string; body: string | null }

function stubAPI(handler: (call: Call) => Response | Promise<Response>) {
  const calls: Call[] = []
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const body = init?.body
    const call: Call = {
      url: String(input),
      method: (init?.method ?? 'GET').toUpperCase(),
      body: body instanceof FormData ? `form:${(body.get('file') as File | null)?.name ?? ''}` : body ? String(body) : null,
    }
    calls.push(call)
    return handler(call)
  })
  vi.stubGlobal('fetch', fetchMock)
  return calls
}

function open(props: Partial<Parameters<typeof Composer>[0]> = {}) {
  const onClose = vi.fn()
  const onSent = vi.fn()
  render(<Composer accounts={accounts} replyTo={null} initialDraft={null} onClose={onClose} onSent={onSent} {...props} />)
  return { onClose, onSent }
}

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement
const sendButton = () => screen.getByRole('button', { name: /发送/ })

describe('composer prefill', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  it('starts empty on a fresh compose, with the first account selected', () => {
    open()
    expect(field('收件人').value).toBe('')
    expect(field('主题').value).toBe('')
    expect(screen.getByRole('combobox')).toHaveValue('1')
    // Nothing saved yet, and the button must not offer to send an empty address.
    expect(screen.getByText('尚未保存')).toBeInTheDocument()
    expect(sendButton()).toBeDisabled()
  })

  it('addresses a reply to the sender and prefixes the subject once', () => {
    open({ replyTo: message() })
    // The display name is stripped; only the address is usable as a recipient.
    expect(field('收件人').value).toBe('ada@example.com')
    expect(field('主题').value).toBe('Re: 发布计划')
    // The quoted original is the snippet, which is all a feed row carries.
    expect(screen.getByPlaceholderText('写点什么…')).toHaveValue('\n\n--- 原邮件 ---\n周五之前给我答复')
    expect(sendButton()).toBeEnabled()
  })

  it('does not stack Re: when replying to a reply', () => {
    cleanup()
    open({ replyTo: message({ subject: 'Re: 发布计划' }) })
    expect(field('主题').value).toBe('Re: 发布计划')
    cleanup()
    open({ replyTo: message({ subject: 're:  发布计划' }) })
    expect(field('主题').value).toBe('Re: 发布计划')
  })

  it('leaves the recipient empty when the sender header carries no address', () => {
    open({ replyTo: message({ sender: 'no-reply' }) })
    expect(field('收件人').value).toBe('')
    // Nothing to reply to: sending would fail server-side, so the button is closed.
    expect(sendButton()).toBeDisabled()
  })

  it('restores every field of a reopened draft and pins its account', () => {
    open({
      initialDraft: draft({
        account_id: 2, to: JSON.stringify(['a@example.com', 'b@example.com']),
        cc: JSON.stringify(['c@example.com']), bcc: JSON.stringify(['d@example.com']),
        subject: '季度报告', body_text: '见附件',
      }),
    })
    expect(field('收件人').value).toBe('a@example.com, b@example.com')
    expect(field('抄送').value).toBe('c@example.com')
    expect(field('密送').value).toBe('d@example.com')
    expect(field('主题').value).toBe('季度报告')
    expect(screen.getByPlaceholderText('写点什么…')).toHaveValue('见附件')
    // The account is fixed once a draft exists: moving it would orphan the remote
    // copy that was appended under the original account.
    const selector = screen.getByRole('combobox')
    expect(selector).toHaveValue('2')
    expect(selector).toBeDisabled()
  })

  it('tolerates address lists that are not decodable', () => {
    open({ initialDraft: draft({ to: 'not json', cc: '{}', bcc: '' }) })
    expect(field('收件人').value).toBe('')
    expect(field('抄送').value).toBe('')
    expect(field('密送').value).toBe('')
  })

  it('lets the account be chosen while no draft exists yet', () => {
    open()
    const selector = screen.getByRole('combobox')
    expect(selector).toBeEnabled()
    // The nameless account falls back to its address in the list.
    expect(screen.getByRole('option', { name: 'alt@163.com' })).toBeInTheDocument()
    fireEvent.change(selector, { target: { value: '2' } })
    expect(selector).toHaveValue('2')
  })

  it('refuses to send on whitespace alone', () => {
    open()
    fireEvent.change(field('收件人'), { target: { value: '   ' } })
    expect(sendButton()).toBeDisabled()
    fireEvent.change(field('收件人'), { target: { value: ' a@example.com ' } })
    expect(sendButton()).toBeEnabled()
  })

  it('closes without saving', () => {
    const calls = stubAPI(() => json({}))
    const { onClose } = open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    expect(onClose).toHaveBeenCalledTimes(1)
    expect(calls).toHaveLength(0)
  })
})

describe('composer attachments', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  function pick(name: string) {
    const file = new File(['payload'], name, { type: 'application/pdf' })
    fireEvent.change(screen.getByLabelText('添加附件'), { target: { files: [file] } })
  }

  it('creates the draft it needs before uploading, and reports the filename', async () => {
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/drafts' && call.method === 'POST') return json(draft({ id: 601, revision: 1 }), 201)
      if (call.url === '/api/v1/drafts/601/attachments') return json({ id: 1, filename: '报告.pdf' }, 201)
      return json({})
    })
    open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    pick('报告.pdf')

    expect(await screen.findByText('已添加 报告.pdf')).toBeInTheDocument()
    expect(calls.map(call => `${call.method} ${call.url}`)).toEqual([
      // The attachment needs an id, so the draft is created first rather than the
      // upload being dropped.
      'POST /api/v1/drafts', 'POST /api/v1/drafts/601/attachments',
    ])
    expect(calls[1].body).toBe('form:报告.pdf')
  })

  it('updates the existing draft instead of creating a second one', async () => {
    const calls = stubAPI(call => {
      if (call.method === 'PATCH') return json(draft({ id: 501, revision: 4 }))
      if (call.url === '/api/v1/drafts/501/attachments') return json({ id: 2 }, 201)
      return json({})
    })
    open({ initialDraft: draft() })
    pick('a.pdf')

    await screen.findByText('已添加 a.pdf')
    expect(calls.filter(call => call.url === '/api/v1/drafts' && call.method === 'POST')).toHaveLength(0)
    expect(calls[0].method).toBe('PATCH')
  })

  it('does nothing when the picker is dismissed', async () => {
    const calls = stubAPI(() => json(draft({ id: 603, revision: 1 }), 201))
    open()
    fireEvent.change(screen.getByLabelText('添加附件'), { target: { files: [] } })
    // No file means no draft either: creating one here would leave an empty draft
    // in the outbox every time the picker is opened and cancelled. The save runs in
    // a microtask, so settle the queue before concluding nothing happened.
    await Promise.resolve()
    await Promise.resolve()
    expect(calls).toHaveLength(0)
    expect(screen.getByText('尚未保存')).toBeInTheDocument()
  })

  it('reports a rejected upload and keeps the window open', async () => {
    stubAPI(call => {
      if (call.url === '/api/v1/drafts' && call.method === 'POST') return json(draft({ id: 602, revision: 1 }), 201)
      return json({ error: { code: 'payload_too_large', message: '附件超过大小限制' } }, 413)
    })
    open()
    pick('big.bin')

    expect(await screen.findByText('附件超过大小限制')).toBeInTheDocument()
    // The typed body survives a rejected attachment.
    expect(screen.getByPlaceholderText('写点什么…')).toBeInTheDocument()
  })

  it('reports a failure to create the draft the upload needed', async () => {
    stubAPI(() => json({ error: { code: 'internal', message: '保存草稿失败' } }, 500))
    open()
    pick('a.pdf')
    expect(await screen.findByText('保存草稿失败')).toBeInTheDocument()
  })
})

describe('composer send failures', () => {
  afterEach(cleanup)
  beforeEach(() => {
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  it('reports a rejected send and re-enables the button', async () => {
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/drafts' && call.method === 'POST') return json(draft({ id: 701, revision: 1 }), 201)
      return json({ error: { code: 'unprocessable_entity', message: '收件人地址无效' } }, 422)
    })
    const { onSent } = open()
    fireEvent.change(field('收件人'), { target: { value: 'not-an-address' } })
    fireEvent.click(sendButton())

    expect(await screen.findByText('收件人地址无效')).toBeInTheDocument()
    expect(onSent).not.toHaveBeenCalled()
    // busy released, so the address can be fixed and sent again.
    await waitFor(() => expect(sendButton()).toBeEnabled())
    expect(calls.map(call => `${call.method} ${call.url}`)).toEqual([
      'POST /api/v1/drafts', 'POST /api/v1/drafts/701/send',
    ])
  })

  it('reports a failed save without attempting the send', async () => {
    const calls = stubAPI(() => json({ error: { code: 'internal', message: '保存失败' } }, 500))
    const { onSent } = open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    fireEvent.click(sendButton())

    expect(await screen.findByText('保存失败')).toBeInTheDocument()
    expect(onSent).not.toHaveBeenCalled()
    // Sending a draft that was never stored would send an empty message.
    expect(calls.some(call => call.url.endsWith('/send'))).toBe(false)
  })

  it('reports a conflict from the optimistic-concurrency save', async () => {
    stubAPI(call => call.method === 'PATCH'
      ? json({ error: { code: 'conflict', message: '草稿已在别处修改' } }, 409)
      : json({}))
    open({ initialDraft: draft() })
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    fireEvent.click(sendButton())
    expect(await screen.findByText('草稿已在别处修改')).toBeInTheDocument()
  })

  it('recovers on a second attempt after one failed save', async () => {
    let attempt = 0
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/drafts' && call.method === 'POST') {
        attempt += 1
        return attempt === 1
          ? json({ error: { code: 'internal', message: '保存失败' } }, 500)
          : json(draft({ id: 702, revision: 1 }), 201)
      }
      return json({ id: 702, status: 'queued' }, 202)
    })
    const { onSent } = open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    fireEvent.click(sendButton())
    expect(await screen.findByText('保存失败')).toBeInTheDocument()

    // A rejected save must not wedge the chain: the retry creates the draft.
    await waitFor(() => expect(sendButton()).toBeEnabled())
    fireEvent.click(sendButton())
    await waitFor(() => expect(onSent).toHaveBeenCalledTimes(1))
    expect(calls.filter(call => call.url === '/api/v1/drafts' && call.method === 'POST')).toHaveLength(2)
  })

  it('reports a transport failure rather than hanging on the spinner', async () => {
    stubAPI(() => { throw new TypeError('Failed to fetch') })
    open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    fireEvent.click(sendButton())
    expect(await screen.findByText(/Failed to fetch/)).toBeInTheDocument()
    await waitFor(() => expect(sendButton()).toBeEnabled())
  })
})

describe('composer autosave feedback', () => {
  afterEach(() => { cleanup(); vi.useRealTimers() })
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    sessionStorage.clear()
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.unstubAllGlobals()
  })

  it('distinguishes a local save from one the provider has taken', async () => {
    stubAPI(() => json(draft({ id: 801, revision: 1, remote_sync_state: 'dirty' }), 201))
    open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)
    expect(await screen.findByText('已保存，等待远端同步')).toBeInTheDocument()
  })

  it('reports the remote copy once the provider has it', async () => {
    stubAPI(() => json(draft({ id: 802, revision: 1, remote_sync_state: 'synced' }), 201))
    open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)
    expect(await screen.findByText('已同步到远端')).toBeInTheDocument()
  })

  it('surfaces a failed autosave in place of the saved state', async () => {
    stubAPI(() => json({ error: { code: 'internal', message: '自动保存失败' } }, 500))
    open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    await vi.advanceTimersByTimeAsync(2100)
    expect(await screen.findByText('自动保存失败')).toBeInTheDocument()
  })

  it('does not autosave when no account can own the draft', async () => {
    const calls = stubAPI(() => json({}))
    render(<Composer accounts={[]} replyTo={null} initialDraft={null} onClose={() => undefined} onSent={() => undefined} />)
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    await vi.advanceTimersByTimeAsync(3000)
    expect(calls).toHaveLength(0)
    expect(screen.getByText('尚未保存')).toBeInTheDocument()
  })

  it('cancels the pending autosave when the window closes', async () => {
    const calls = stubAPI(() => json(draft(), 201))
    const view = render(<Composer accounts={accounts} replyTo={null} initialDraft={null} onClose={() => undefined} onSent={() => undefined} />)
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    view.unmount()
    await vi.advanceTimersByTimeAsync(3000)
    // A save landing after the composer is gone would revive a discarded draft.
    expect(calls).toHaveLength(0)
  })

  it('does not revive the draft with an autosave queued before the send', async () => {
    const calls = stubAPI(call => {
      if (call.url === '/api/v1/drafts' && call.method === 'POST') return json(draft({ id: 803, revision: 1 }), 201)
      return json({ id: 803, status: 'queued' }, 202)
    })
    const { onSent } = open()
    fireEvent.change(field('收件人'), { target: { value: 'a@example.com' } })
    // Typing arms the 2s timer; sending inside that window must disarm it.
    await vi.advanceTimersByTimeAsync(500)
    fireEvent.click(sendButton())
    await waitFor(() => expect(onSent).toHaveBeenCalled())
    await vi.advanceTimersByTimeAsync(3000)

    expect(calls.filter(call => call.url === '/api/v1/drafts' && call.method === 'POST')).toHaveLength(1)
    expect(calls.filter(call => call.method === 'PATCH')).toHaveLength(0)
  })
})
