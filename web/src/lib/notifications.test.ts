import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// These are the degraded paths of the notification module: no service worker, a
// revoked permission, a throwing constructor, a clipboard that refuses. They are
// reached through the module directly rather than through App, because each one
// needs a different browser shape and the interesting ones are environments App's
// harness cannot represent — a browser with no serviceWorker at all, or an insecure
// context. The happy path stays covered end-to-end in otp.test.tsx.
//
// Every test imports the module freshly: registerServiceWorker caches the
// registration in module scope, and a leaked cache would let one test's worker
// satisfy the next test's "no worker" premise.
async function load() {
  vi.resetModules()
  return import('./notifications')
}

type NotificationStub = { permission?: string; maxActions?: number; throwOnMaxActions?: boolean }

// The implementation must be a function expression, never an arrow: the module
// reaches its fallback with `new Notification(...)`, and a mock wrapping an arrow
// throws "not a constructor" on `new`. That would make the fallback look broken
// here while working in every browser.
function stubNotification(options: NotificationStub = {}, impl: () => void = function () {}) {
  const constructor = vi.fn(impl)
  const stub = Object.assign(constructor, { permission: options.permission ?? 'granted' })
  if (options.throwOnMaxActions) {
    // A getter that throws stands in for an engine where touching maxActions is
    // not safe; the module guards it precisely because the value is non-standard.
    Object.defineProperty(stub, 'maxActions', { get() { throw new Error('unavailable') } })
  } else if (options.maxActions !== undefined) {
    Object.assign(stub, { maxActions: options.maxActions })
  }
  vi.stubGlobal('Notification', stub)
  return constructor
}

function setSecure(value: boolean) {
  Object.defineProperty(window, 'isSecureContext', { value, configurable: true })
}

function setServiceWorker(value: unknown) {
  if (value === undefined) {
    // delete rather than set to undefined: the module tests with `in`, so an
    // own property holding undefined still reads as supported.
    delete (navigator as { serviceWorker?: unknown }).serviceWorker
    return
  }
  Object.defineProperty(navigator, 'serviceWorker', { value, configurable: true })
}

const originalServiceWorker = Object.getOwnPropertyDescriptor(navigator, 'serviceWorker')

afterEach(() => {
  vi.unstubAllGlobals()
  if (originalServiceWorker) Object.defineProperty(navigator, 'serviceWorker', originalServiceWorker)
  else setServiceWorker(undefined)
  document.body.innerHTML = ''
})

beforeEach(() => {
  setSecure(true)
})

describe('registerServiceWorker', () => {
  it('returns null in an insecure context without touching the API', async () => {
    const register = vi.fn()
    setServiceWorker({ register })
    setSecure(false)
    const { registerServiceWorker } = await load()

    expect(await registerServiceWorker()).toBeNull()
    // The point of the guard: registration on plain HTTP throws, and the feature is
    // meant to be absent there rather than noisy.
    expect(register).not.toHaveBeenCalled()
  })

  it('returns null where the browser has no service worker at all', async () => {
    setServiceWorker(undefined)
    const { registerServiceWorker } = await load()
    expect(await registerServiceWorker()).toBeNull()
  })

  it('returns null when registration is rejected', async () => {
    setServiceWorker({ register: vi.fn(async () => { throw new Error('blocked by policy') }) })
    const { registerServiceWorker } = await load()
    expect(await registerServiceWorker()).toBeNull()
  })

  it('returns the registration on success', async () => {
    const registration = { showNotification: vi.fn() }
    setServiceWorker({ register: vi.fn(async () => registration) })
    const { registerServiceWorker } = await load()
    expect(await registerServiceWorker()).toBe(registration)
  })
})

describe('showOTPNotification', () => {
  it('refuses an empty code', async () => {
    const constructor = stubNotification()
    setServiceWorker({ getRegistration: vi.fn() })
    const { showOTPNotification } = await load()

    expect(await showOTPNotification('', 'subject', 1)).toBe(false)
    expect(constructor).not.toHaveBeenCalled()
  })

  it('refuses when permission is not granted', async () => {
    const constructor = stubNotification({ permission: 'denied' })
    const getRegistration = vi.fn()
    setServiceWorker({ getRegistration })
    const { showOTPNotification } = await load()

    expect(await showOTPNotification('482913', 'subject', 1)).toBe(false)
    expect(constructor).not.toHaveBeenCalled()
    expect(getRegistration).not.toHaveBeenCalled()
  })

  it('survives a Notification global that throws on access', async () => {
    // Some privacy-hardened browsers make even the permission read throw. That must
    // not propagate: the caller is a socket message handler.
    vi.stubGlobal('Notification', undefined)
    setServiceWorker({ getRegistration: vi.fn() })
    const { showOTPNotification } = await load()
    expect(await showOTPNotification('482913', 'subject', 1)).toBe(false)
  })

  it('falls back to a plain notification when no worker is registered', async () => {
    const constructor = stubNotification({ maxActions: 2 })
    setServiceWorker({ getRegistration: vi.fn(async () => undefined) })
    const { showOTPNotification } = await load()

    expect(await showOTPNotification('482913', '【银行】验证码', 7)).toBe(true)
    expect(constructor).toHaveBeenCalledTimes(1)
    const [title, options] = constructor.mock.calls[0] as unknown as [string, NotificationOptions]
    expect(title).toBe('收到验证码')
    // The code has to be in the body, because this is the variant with no copy
    // button: reading it off the notification is the only way to use it.
    expect(options.body).toContain('482913')
    expect(options.tag).toBe('otp-7')
  })

  it('falls back to a plain notification when showNotification rejects', async () => {
    const constructor = stubNotification({ maxActions: 2 })
    const showNotification = vi.fn(async () => { throw new Error('actions unsupported') })
    setServiceWorker({ getRegistration: vi.fn(async () => ({ showNotification })) })
    const { showOTPNotification } = await load()

    expect(await showOTPNotification('482913', 'subject', 7)).toBe(true)
    expect(showNotification).toHaveBeenCalledTimes(1)
    expect(constructor).toHaveBeenCalledTimes(1)
  })

  it('reports failure when both paths throw', async () => {
    stubNotification({ maxActions: 0 }, function () { throw new Error('construction refused') })
    setServiceWorker({ getRegistration: vi.fn(async () => undefined) })
    const { showOTPNotification } = await load()

    expect(await showOTPNotification('482913', 'subject', 7)).toBe(false)
  })

  it('omits actions when the engine reports none, and keeps them when it does', async () => {
    stubNotification({ maxActions: 0 })
    const withoutActions = vi.fn(async () => undefined)
    setServiceWorker({ getRegistration: vi.fn(async () => ({ showNotification: withoutActions })) })
    const first = await load()
    expect(await first.showOTPNotification('482913', 'subject', 7)).toBe(true)
    expect((withoutActions.mock.calls[0] as unknown as [string, { actions?: unknown }])[1].actions).toBeUndefined()

    stubNotification({ maxActions: 2 })
    const withActions = vi.fn(async () => undefined)
    setServiceWorker({ getRegistration: vi.fn(async () => ({ showNotification: withActions })) })
    const second = await load()
    expect(await second.showOTPNotification('482913', 'subject', 7)).toBe(true)
    const options = (withActions.mock.calls[0] as unknown as [string, { actions?: { action: string }[] }])[1]
    expect(options.actions).toEqual([{ action: 'copy', title: '复制验证码' }])
  })

  it('omits actions when reading maxActions throws', async () => {
    stubNotification({ throwOnMaxActions: true })
    const showNotification = vi.fn(async () => undefined)
    setServiceWorker({ getRegistration: vi.fn(async () => ({ showNotification })) })
    const { showOTPNotification } = await load()

    expect(await showOTPNotification('482913', 'subject', 7)).toBe(true)
    const options = (showNotification.mock.calls[0] as unknown as [string, { actions?: unknown }])[1]
    expect(options.actions).toBeUndefined()
  })

  it('substitutes a placeholder for a missing subject', async () => {
    stubNotification({ maxActions: 2 })
    const showNotification = vi.fn(async () => undefined)
    setServiceWorker({ getRegistration: vi.fn(async () => ({ showNotification })) })
    const { showOTPNotification } = await load()

    await showOTPNotification('482913', '', 7)
    const options = (showNotification.mock.calls[0] as unknown as [string, { body: string }])[1]
    expect(options.body).toBe('482913\n（无主题）')
  })

  it('reuses the registration cached by registerServiceWorker', async () => {
    const showNotification = vi.fn(async () => undefined)
    const getRegistration = vi.fn(async () => undefined)
    setServiceWorker({ register: vi.fn(async () => ({ showNotification })), getRegistration })
    stubNotification({ maxActions: 2 })
    const { registerServiceWorker, showOTPNotification } = await load()

    await registerServiceWorker()
    expect(await showOTPNotification('482913', 'subject', 7)).toBe(true)
    expect(showNotification).toHaveBeenCalledTimes(1)
    // Already held, so no second lookup: getRegistration is the fallback only.
    expect(getRegistration).not.toHaveBeenCalled()
  })
})

describe('copyText', () => {
  it('refuses an empty value', async () => {
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const { copyText } = await load()

    expect(await copyText('')).toBe(false)
    expect(writeText).not.toHaveBeenCalled()
  })

  it('uses the async clipboard when it works', async () => {
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const { copyText } = await load()

    expect(await copyText('482913')).toBe(true)
    expect(writeText).toHaveBeenCalledWith('482913')
  })

  it('falls back to execCommand when the clipboard is denied', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: vi.fn(async () => { throw new Error('denied') }) }, configurable: true,
    })
    // The value has to reach the field execCommand copies from, so it is read back
    // here rather than only asserting the return.
    let copied = ''
    const execCommand = vi.fn(() => {
      copied = (document.activeElement as HTMLTextAreaElement | null)?.value
        ?? (document.querySelector('textarea')?.value ?? '')
      return true
    })
    Object.defineProperty(document, 'execCommand', { value: execCommand, configurable: true })
    const { copyText } = await load()

    expect(await copyText('482913')).toBe(true)
    expect(copied).toBe('482913')
    // And the scratch field must not be left behind.
    expect(document.querySelectorAll('textarea')).toHaveLength(0)
  })

  it('reports failure when execCommand declines', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: vi.fn(async () => { throw new Error('denied') }) }, configurable: true,
    })
    Object.defineProperty(document, 'execCommand', { value: vi.fn(() => false), configurable: true })
    const { copyText } = await load()

    expect(await copyText('482913')).toBe(false)
    expect(document.querySelectorAll('textarea')).toHaveLength(0)
  })

  it('leaves no scratch field behind when execCommand throws', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: vi.fn(async () => { throw new Error('denied') }) }, configurable: true,
    })
    Object.defineProperty(document, 'execCommand', {
      value: vi.fn(() => { throw new Error('unsupported') }), configurable: true,
    })
    const { copyText } = await load()

    expect(await copyText('482913')).toBe(false)
    // Without the finally this accumulates one hidden textarea per attempt, so the
    // count is checked after several.
    expect(await copyText('482913')).toBe(false)
    expect(await copyText('482913')).toBe(false)
    expect(document.querySelectorAll('textarea')).toHaveLength(0)
  })

  it('reports failure when there is no clipboard API at all', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })
    Object.defineProperty(document, 'execCommand', { value: vi.fn(() => false), configurable: true })
    const { copyText } = await load()
    expect(await copyText('482913')).toBe(false)
  })
})

describe('listenForCopyRequests', () => {
  function worker(ready: Promise<unknown>) {
    const listeners = new Set<(event: MessageEvent) => void>()
    return {
      api: {
        ready,
        addEventListener: (_t: string, h: (event: MessageEvent) => void) => { listeners.add(h) },
        removeEventListener: (_t: string, h: (event: MessageEvent) => void) => { listeners.delete(h) },
      },
      listeners,
      send: (data: unknown) => listeners.forEach(h => h({ data } as MessageEvent)),
    }
  }

  it('returns an inert unsubscribe where service workers are unsupported', async () => {
    setServiceWorker(undefined)
    const { listenForCopyRequests } = await load()
    const onCopy = vi.fn()

    const stop = listenForCopyRequests(onCopy)
    expect(() => stop()).not.toThrow()
    expect(onCopy).not.toHaveBeenCalled()
  })

  it('claims a parked code once the worker is ready', async () => {
    const postMessage = vi.fn()
    const { api } = worker(Promise.resolve({ active: { postMessage } }))
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()

    listenForCopyRequests(vi.fn())
    await Promise.resolve()
    await Promise.resolve()
    // This is how a code actioned with no tab open reaches the page: the worker
    // parks it rather than putting it in the URL, where it would enter history.
    expect(postMessage).toHaveBeenCalledWith({ type: 'CLAIM_OTP' })
  })

  it('survives a ready promise that rejects', async () => {
    const { api } = worker(Promise.reject(new Error('worker gone')))
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()

    // An unhandled rejection here would fail the suite, which is the assertion.
    expect(() => listenForCopyRequests(vi.fn())).not.toThrow()
    await Promise.resolve()
    await Promise.resolve()
  })

  it('survives a ready worker with no active instance', async () => {
    const { api } = worker(Promise.resolve({ active: null }))
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()
    expect(() => listenForCopyRequests(vi.fn())).not.toThrow()
    await Promise.resolve()
  })

  it('copies the code from a COPY_OTP message', async () => {
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const { api, send } = worker(Promise.resolve({ active: { postMessage: vi.fn() } }))
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()

    const outcomes: { code: string; copied: boolean }[] = []
    listenForCopyRequests(outcome => outcomes.push(outcome))
    send({ type: 'COPY_OTP', code: '482913' })
    await vi.waitFor(() => expect(outcomes).toHaveLength(1))

    expect(writeText).toHaveBeenCalledWith('482913')
    expect(outcomes[0]).toEqual({ code: '482913', copied: true })
  })

  it('still reports the code when the copy is refused', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: vi.fn(async () => { throw new Error('denied') }) }, configurable: true,
    })
    Object.defineProperty(document, 'execCommand', { value: vi.fn(() => false), configurable: true })
    const { api, send } = worker(Promise.resolve({ active: { postMessage: vi.fn() } }))
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()

    const outcomes: { code: string; copied: boolean }[] = []
    listenForCopyRequests(outcome => outcomes.push(outcome))
    send({ type: 'COPY_OTP', code: '482913' })
    // copied: false is what drives the UI to tell the user to select it manually,
    // so the callback has to fire on failure too.
    await vi.waitFor(() => expect(outcomes).toEqual([{ code: '482913', copied: false }]))
  })

  it('ignores messages that are not a copy request', async () => {
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const { api, send } = worker(Promise.resolve({ active: { postMessage: vi.fn() } }))
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()

    const onCopy = vi.fn()
    listenForCopyRequests(onCopy)
    send(null)
    send({ type: 'SOMETHING_ELSE', code: '482913' })
    send({ type: 'COPY_OTP' })
    send({ type: 'COPY_OTP', code: '' })
    await Promise.resolve()

    expect(onCopy).not.toHaveBeenCalled()
    expect(writeText).not.toHaveBeenCalled()
  })

  it('stops delivering after unsubscribe', async () => {
    const { api, send, listeners } = worker(Promise.resolve({ active: { postMessage: vi.fn() } }))
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: vi.fn(async () => undefined) }, configurable: true })
    setServiceWorker(api)
    const { listenForCopyRequests } = await load()

    const onCopy = vi.fn()
    const stop = listenForCopyRequests(onCopy)
    stop()
    expect(listeners.size).toBe(0)
    send({ type: 'COPY_OTP', code: '482913' })
    await Promise.resolve()
    expect(onCopy).not.toHaveBeenCalled()
  })
})
