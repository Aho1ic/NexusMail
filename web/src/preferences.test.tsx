import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsDialog } from './components/SettingsDialog'
import {
  defaultPreferences, loadPreferences, notificationPermission,
  requestNotificationPermission, savePreferences, type Preferences,
} from './lib/preferences'

// Preferences are read on every render and written on every toggle, so a storage
// failure or a value left over from an older shape must degrade to the documented
// default rather than rendering the app with `undefined` settings — which reads as
// "off" and would silently disable notifications the user had enabled.

const storageKey = 'nexusmail.preferences'

function withStorage(overrides: Partial<Storage>) {
  const original = globalThis.localStorage
  Object.defineProperty(globalThis, 'localStorage', { value: { ...original, ...overrides }, configurable: true })
  return () => Object.defineProperty(globalThis, 'localStorage', { value: original, configurable: true })
}

describe('preferences storage', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.unstubAllGlobals()
  })

  it('returns the documented defaults when nothing is stored', () => {
    expect(loadPreferences()).toEqual(defaultPreferences)
    // Remote images stay blocked until asked: the default is a privacy decision.
    expect(defaultPreferences.autoLoadRemoteImages).toBe(false)
    expect(defaultPreferences.desktopNotifications).toBe(true)
  })

  it('round-trips every field', () => {
    const stored: Preferences = {
      desktopNotifications: false, verificationCodeNotifications: false,
      autoLoadRemoteImages: true, keyboardShortcuts: false,
    }
    savePreferences(stored)
    expect(loadPreferences()).toEqual(stored)
  })

  it('falls back per field when the stored shape is older or wrong', () => {
    // One field known and honoured, two of the wrong type, one key since removed.
    // The wrong-typed values are chosen so that coercing them by truthiness would
    // give the opposite of the default: silently flipping a setting is the bug.
    localStorage.setItem(storageKey, JSON.stringify({
      autoLoadRemoteImages: true, desktopNotifications: 0, keyboardShortcuts: 'no', removedSetting: 1,
    }))
    expect(loadPreferences()).toEqual({
      ...defaultPreferences,
      autoLoadRemoteImages: true,
      // 0 is not `false` and 'no' is not `true`: neither may reach a setting.
      desktopNotifications: true,
      keyboardShortcuts: true,
    })

    // And the same in the other direction, so the check cannot be "default wins".
    localStorage.setItem(storageKey, JSON.stringify({ autoLoadRemoteImages: 'yes' }))
    expect(loadPreferences().autoLoadRemoteImages).toBe(false)
  })

  it('falls back to defaults for unparseable and non-object values', () => {
    localStorage.setItem(storageKey, 'not json')
    expect(loadPreferences()).toEqual(defaultPreferences)
    localStorage.setItem(storageKey, 'null')
    expect(loadPreferences()).toEqual(defaultPreferences)
    localStorage.setItem(storageKey, '"a string"')
    expect(loadPreferences()).toEqual(defaultPreferences)
    localStorage.setItem(storageKey, '[]')
    expect(loadPreferences()).toEqual(defaultPreferences)
  })

  it('survives storage that throws, in both directions', () => {
    // Private browsing and blocked-storage modes throw instead of returning null.
    const restoreRead = withStorage({ getItem: () => { throw new DOMException('denied') } })
    expect(loadPreferences()).toEqual(defaultPreferences)
    restoreRead()

    const restoreWrite = withStorage({ setItem: () => { throw new DOMException('quota') } })
    // Best-effort: a failed write must not take down the toggle that caused it.
    expect(() => savePreferences(defaultPreferences)).not.toThrow()
    restoreWrite()
  })
})

describe('notification permission', () => {
  beforeEach(() => { vi.unstubAllGlobals() })

  it('reports unsupported where the API is absent', () => {
    // jsdom ships no Notification, which is the same shape as a plain-HTTP origin.
    expect('Notification' in window).toBe(false)
    expect(notificationPermission()).toBe('unsupported')
  })

  it('reports the browser value when the API is present', () => {
    vi.stubGlobal('Notification', { permission: 'granted' })
    expect(notificationPermission()).toBe('granted')
    vi.stubGlobal('Notification', { permission: 'denied' })
    expect(notificationPermission()).toBe('denied')
  })

  it('reports unsupported when reading the permission throws', () => {
    vi.stubGlobal('Notification', { get permission(): string { throw new Error('blocked by policy') } })
    expect(notificationPermission()).toBe('unsupported')
  })

  it('requests permission and passes the answer back', async () => {
    vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => 'granted' })
    await expect(requestNotificationPermission()).resolves.toBe('granted')
  })

  it('reports unsupported when the request is absent or rejects', async () => {
    await expect(requestNotificationPermission()).resolves.toBe('unsupported')
    vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => { throw new Error('gesture required') } })
    await expect(requestNotificationPermission()).resolves.toBe('unsupported')
  })
})

describe('settings dialog', () => {
  afterEach(cleanup)
  beforeEach(() => { localStorage.clear(); vi.unstubAllGlobals() })

  function open(overrides: Partial<Preferences> = {}, accounts: Parameters<typeof SettingsDialog>[0]['accounts'] = []) {
    const onChange = vi.fn()
    const onClose = vi.fn()
    const onAddAccount = vi.fn()
    const onLogout = vi.fn()
    render(<SettingsDialog preferences={{ ...defaultPreferences, ...overrides }} accounts={accounts}
      onChange={onChange} onClose={onClose} onAddAccount={onAddAccount} onLogout={onLogout} />)
    return { onChange, onClose, onAddAccount, onLogout }
  }

  it('offers the authorization prompt only while the decision is open', () => {
    vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => 'granted' })
    open()
    expect(screen.getByRole('button', { name: '授权' })).toBeInTheDocument()
    expect(screen.queryByText(/已拒绝通知权限/)).not.toBeInTheDocument()

    cleanup()
    vi.stubGlobal('Notification', { permission: 'granted' })
    open()
    // Nothing to ask and nothing to warn about.
    expect(screen.queryByRole('button', { name: '授权' })).not.toBeInTheDocument()
    expect(screen.queryByText(/已拒绝通知权限/)).not.toBeInTheDocument()
    expect(screen.queryByText(/不支持桌面通知/)).not.toBeInTheDocument()
  })

  it('explains a denied permission instead of offering a button that cannot work', () => {
    vi.stubGlobal('Notification', { permission: 'denied' })
    open()
    expect(screen.getByText(/已拒绝通知权限/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '授权' })).not.toBeInTheDocument()
  })

  it('explains an unsupported environment', () => {
    // No Notification at all: the whole feature is absent, not broken.
    open()
    expect(screen.getByText(/不支持桌面通知/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '授权' })).not.toBeInTheDocument()
  })

  it('replaces the prompt with the outcome once permission is granted', async () => {
    let resolve: (value: string) => void = () => undefined
    vi.stubGlobal('Notification', {
      permission: 'default',
      requestPermission: () => new Promise<string>(done => { resolve = done }),
    })
    open()
    fireEvent.click(screen.getByRole('button', { name: '授权' }))
    // In flight: the button reports progress and cannot be pressed twice.
    const pending = await screen.findByRole('button', { name: '请求中…' })
    expect(pending).toBeDisabled()

    resolve('granted')
    await waitFor(() => expect(screen.queryByRole('button', { name: '请求中…' })).not.toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '授权' })).not.toBeInTheDocument()
  })

  it('shows the denial when the user refuses at the prompt', async () => {
    vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => 'denied' })
    open()
    fireEvent.click(screen.getByRole('button', { name: '授权' }))
    expect(await screen.findByText(/已拒绝通知权限/)).toBeInTheDocument()
  })

  it('replaces the prompt with the explanation when the request throws', async () => {
    vi.stubGlobal('Notification', { permission: 'default', requestPermission: async () => { throw new Error('no user gesture') } })
    open()
    fireEvent.click(screen.getByRole('button', { name: '授权' }))
    // requestNotificationPermission maps the throw to 'unsupported', so the prompt
    // is replaced by the explanation rather than staying stuck on 请求中….
    expect(await screen.findByText(/不支持桌面通知/)).toBeInTheDocument()
  })

  it('re-enables the prompt when the user dismisses it without deciding', async () => {
    // Dismissing the browser prompt resolves to 'default' again, and the button is
    // the only way to raise it a second time — so it has to come back enabled.
    let resolve: (value: string) => void = () => undefined
    vi.stubGlobal('Notification', {
      permission: 'default',
      requestPermission: () => new Promise<string>(done => { resolve = done }),
    })
    open()
    fireEvent.click(screen.getByRole('button', { name: '授权' }))
    expect(await screen.findByRole('button', { name: '请求中…' })).toBeDisabled()

    resolve('default')
    const again = await screen.findByRole('button', { name: '授权' })
    expect(again).toBeEnabled()
  })

  it('emits one patch per toggle, carrying the flipped value', () => {
    const { onChange } = open({ desktopNotifications: true, verificationCodeNotifications: true, autoLoadRemoteImages: false, keyboardShortcuts: true })
    const toggles: Array<[string, Partial<Preferences>]> = [
      ['新邮件桌面通知', { desktopNotifications: false }],
      ['验证码通知', { verificationCodeNotifications: false }],
      ['自动加载远程图片', { autoLoadRemoteImages: true }],
      ['启用单键快捷键', { keyboardShortcuts: false }],
    ]
    for (const [label, patch] of toggles) {
      const control = screen.getByRole('switch', { name: label })
      // The switch reports its own state, which is what a screen reader announces.
      expect(control).toHaveAttribute('aria-checked', String(!Object.values(patch)[0]))
      fireEvent.click(control)
      expect(onChange).toHaveBeenLastCalledWith(patch)
    }
    expect(onChange).toHaveBeenCalledTimes(4)
  })

  it('closes on Escape, on the header button, and on 完成', () => {
    const first = open()
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(first.onClose).toHaveBeenCalledTimes(1)
    // Any other key must not dismiss a dialog holding settings.
    fireEvent.keyDown(window, { key: 'a' })
    expect(first.onClose).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: '关闭设置' }))
    fireEvent.click(screen.getByRole('button', { name: '完成' }))
    expect(first.onClose).toHaveBeenCalledTimes(3)
  })

  it('stops listening for Escape once unmounted', () => {
    const onClose = vi.fn()
    const view = render(<SettingsDialog preferences={defaultPreferences} accounts={[]}
      onChange={() => undefined} onClose={onClose} onAddAccount={() => undefined} onLogout={() => undefined} />)
    view.unmount()
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()
  })

  it('routes the account and logout actions without acting itself', () => {
    const { onAddAccount, onLogout } = open()
    expect(screen.getByText('还没有连接任何邮箱。')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /连接邮箱/ }))
    fireEvent.click(screen.getByRole('button', { name: /退出登录/ }))
    expect(onAddAccount).toHaveBeenCalledTimes(1)
    expect(onLogout).toHaveBeenCalledTimes(1)
  })

  it('reports each account status in the reader’s language', () => {
    open({}, [
      { id: 1, email: 'a@qq.com', display_name: '主号', provider: 'qq', status: 'connected', last_connected_at: Date.UTC(2026, 0, 2, 3, 4) },
      { id: 2, email: 'b@163.com', display_name: '', provider: '163', status: 'backoff', last_error: '535 authentication failed' },
      { id: 3, email: 'c@gmail.com', display_name: 'G', provider: 'gmail', status: 'disconnected' },
    ])
    // The connected row also carries the last-connected time in the same line.
    expect(screen.getByText(/^已连接 · 最近连接/)).toBeInTheDocument()
    expect(screen.getByText('连接异常，正在重试')).toBeInTheDocument()
    expect(screen.getByText('未连接')).toBeInTheDocument()
    // No display name: the address stands in for it, so it appears as both the
    // heading and the address line rather than leaving an empty row.
    expect(screen.getAllByText('b@163.com')).toHaveLength(2)
    expect(screen.getByText('主号')).toBeInTheDocument()
    // The failure is announced, not just coloured.
    expect(screen.getByRole('alert')).toHaveTextContent('535 authentication failed')
    expect(screen.queryByText('还没有连接任何邮箱。')).not.toBeInTheDocument()
  })
})
