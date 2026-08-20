import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { act } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

class FakeSocket {
  static instances: FakeSocket[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  constructor(public url: string) { FakeSocket.instances.push(this) }
  close() { /* no-op */ }
  emit(type: string, data: Record<string, unknown>) {
    this.onmessage?.({ data: JSON.stringify({ type, sequence: 1, occurred_at: 0, data }) })
  }
}

// jsdom implements neither navigator.serviceWorker nor a writable clipboard, so
// the whole worker surface is faked. showNotification is what the assertions read:
// it is the only API that honours a notification action, which is the copy button.
const showNotification = vi.fn(async () => undefined)
const postMessage = vi.fn()
const workerListeners = new Set<(event: MessageEvent) => void>()
const registration = { showNotification, active: { postMessage } }
const serviceWorker = {
  register: vi.fn(async () => registration),
  getRegistration: vi.fn(async () => registration),
  ready: Promise.resolve(registration),
  addEventListener: (_type: string, handler: (event: MessageEvent) => void) => { workerListeners.add(handler) },
  removeEventListener: (_type: string, handler: (event: MessageEvent) => void) => { workerListeners.delete(handler) },
}

function fromWorker(data: unknown) {
  workerListeners.forEach(handler => handler({ data } as MessageEvent))
}

function json(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), { status, headers: { 'content-type': 'application/json' } })
}

const account = { id: 1, email: 'mail@example.com', display_name: 'Mail', provider: 'qq', status: 'connected' }

const mail = {
  id: 7, account_id: 1, direction: 'incoming', subject: '【示例服务】验证码', sender: 'Robot <robot@example.com>',
  recipients: 'mail@example.com', from: '[]', to: '[]', cc: '[]', bcc: '[]', snippet: '您的验证码是 482913',
  body_state: 'ready', body_text: '您的验证码是 482913，10 分钟内有效。', received_at: 0,
  is_read: true, is_starred: false, has_attachments: false,
}

describe('verification code notifications', () => {
  afterEach(cleanup)

  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    FakeSocket.instances = []
    workerListeners.clear()
    showNotification.mockClear()
    postMessage.mockClear()
    vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
    // Actions and maxActions only exist on a service-worker notification, so the
    // constructor is stubbed purely to observe that the plain fallback stays unused.
    vi.stubGlobal('Notification', Object.assign(vi.fn(), { permission: 'granted', maxActions: 2 }))
    Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true })
    Object.defineProperty(navigator, 'serviceWorker', { value: serviceWorker, configurable: true })
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: vi.fn(async () => undefined) }, configurable: true })
    sessionStorage.setItem('nexusmail.csrf', 'csrf-value')
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/accounts') return json({ items: [account] })
      if (url.includes('/mailboxes')) return json({ items: [] })
      if (/^\/api\/v1\/messages\/\d+$/.test(url)) return json({ message: mail, attachments: [], otp_code: '482913' })
      if (url.startsWith('/api/v1/messages')) return json({ items: [mail] })
      return json({})
    }))
  })

  async function mount() {
    render(<App />)
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1))
    return FakeSocket.instances[0]
  }

  it('raises a notification carrying a copy action when an arrival has a code', async () => {
    const socket = await mount()

    await act(async () => { socket.emit('NEW_EMAIL', { message_id: 7, otp_code: '482913', otp_subject: '【示例服务】验证码' }) })

    await waitFor(() => expect(showNotification).toHaveBeenCalledTimes(1))
    const [title, options] = showNotification.mock.calls[0] as unknown as [string, Record<string, unknown>]
    expect(title).toBe('收到验证码')
    expect(String(options.body)).toContain('482913')
    // The tag lets the body pass replace the arrival guess instead of stacking.
    expect(options.tag).toBe('otp-7')
    expect(options.requireInteraction).toBe(true)
    expect(options.actions).toEqual([{ action: 'copy', title: '复制验证码' }])
    // A code-bearing arrival replaces the generic notice rather than adding to it.
    expect(Notification).not.toHaveBeenCalled()
  })

  it('does not repeat a code it already raised but does follow a better one', async () => {
    const socket = await mount()
    await act(async () => { socket.emit('NEW_EMAIL', { message_id: 7, otp_code: '482913' }) })
    await waitFor(() => expect(showNotification).toHaveBeenCalledTimes(1))

    // The body prefetch republishes the same message; the same code must stay quiet.
    await act(async () => { socket.emit('MESSAGE_UPDATED', { message_id: 7, otp_code: '482913' }) })
    expect(showNotification).toHaveBeenCalledTimes(1)

    // A different code for that message means the subject-only guess was wrong.
    await act(async () => { socket.emit('MESSAGE_UPDATED', { message_id: 7, otp_code: '135790' }) })
    await waitFor(() => expect(showNotification).toHaveBeenCalledTimes(2))
  })

  it('leaves the generic notice in place when code notifications are off', async () => {
    localStorage.setItem('nexusmail.preferences', JSON.stringify({ verificationCodeNotifications: false }))
    const socket = await mount()

    await act(async () => { socket.emit('NEW_EMAIL', { message_id: 7, otp_code: '482913' }) })

    expect(showNotification).not.toHaveBeenCalled()
    expect(Notification).toHaveBeenCalledWith('NexusMail 收到新邮件')
  })

  it('copies the code the worker forwards after the notification button is pressed', async () => {
    await mount()
    await waitFor(() => expect(serviceWorker.register).toHaveBeenCalled())

    await act(async () => { fromWorker({ type: 'COPY_OTP', code: '482913' }) })

    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledWith('482913'))
    expect(await screen.findByRole('status')).toHaveTextContent('已复制验证码 482913')
  })

  it('claims a code the worker parked while no tab was open', async () => {
    await mount()
    await waitFor(() => expect(postMessage).toHaveBeenCalledWith({ type: 'CLAIM_OTP' }))
  })

  it('offers a copy chip in the detail view as the path back to a dismissed code', async () => {
    await mount()
    fireEvent.click(await screen.findByRole('button', { name: /验证码/ }))

    fireEvent.click(await screen.findByRole('button', { name: '复制验证码 482913' }))

    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledWith('482913'))
    expect(await screen.findByRole('status')).toHaveTextContent('已复制验证码 482913')
  })

  it('tells the user the code when the clipboard refuses the write', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: vi.fn(async () => { throw new Error('denied') }) }, configurable: true })
    // execCommand is the documented fallback and is absent in jsdom; both paths
    // failing is exactly the case where the code has to be readable on screen.
    await mount()

    await act(async () => { fromWorker({ type: 'COPY_OTP', code: '482913' }) })

    expect(await screen.findByRole('status')).toHaveTextContent('复制失败，验证码为 482913')
  })
})
