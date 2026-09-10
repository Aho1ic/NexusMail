import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { onOAuthResult, relayOAuthResultToOpener } from './lib/oauthbridge'

// The relay decides whether a window is a consent popup on its way back or the app
// itself. Getting that wrong in either direction is visible: a false positive closes
// the user's mailbox, and a false negative leaves an orphan window open forever with
// the dialog still waiting on it.
//
// The listener is the trust boundary. The consent screen — and anything else holding
// a handle on this window — can post to it, so only our own origin may announce that
// an account was created.

function stubWindow(options: { search: string; opener: boolean }) {
  const postMessage = vi.fn()
  const close = vi.fn()
  vi.stubGlobal('location', { search: options.search, origin: 'http://localhost:3000' })
  vi.stubGlobal('opener', options.opener ? { postMessage } : null)
  vi.stubGlobal('close', close)
  return { postMessage, close }
}

describe('oauth relay', () => {
  beforeEach(() => { vi.unstubAllGlobals() })
  afterEach(() => { vi.unstubAllGlobals() })

  it('hands a success to the opener and closes itself', () => {
    const { postMessage, close } = stubWindow({ search: '?oauth=success', opener: true })

    expect(relayOAuthResultToOpener()).toBe(true)
    expect(postMessage).toHaveBeenCalledWith({ source: 'nexusmail-oauth', status: 'success' }, 'http://localhost:3000')
    expect(close).toHaveBeenCalledTimes(1)
  })

  it('carries the failure reason through', () => {
    const { postMessage } = stubWindow({ search: '?oauth=error&reason=access_denied', opener: true })

    expect(relayOAuthResultToOpener()).toBe(true)
    expect(postMessage).toHaveBeenCalledWith({ source: 'nexusmail-oauth', status: 'error', reason: 'access_denied' }, 'http://localhost:3000')
  })

  it('leaves the app alone when there is no opener', () => {
    // A user who reloads or bookmarks `/?oauth=success` lands here. Closing the
    // only window, or refusing to render, would strand them.
    const { postMessage, close } = stubWindow({ search: '?oauth=success', opener: false })

    expect(relayOAuthResultToOpener()).toBe(false)
    expect(postMessage).not.toHaveBeenCalled()
    expect(close).not.toHaveBeenCalled()
  })

  it('leaves the app alone when the URL carries no outcome', () => {
    // Any window can have an opener — target="_blank" from anywhere.
    const { postMessage, close } = stubWindow({ search: '?folder=inbox', opener: true })

    expect(relayOAuthResultToOpener()).toBe(false)
    expect(postMessage).not.toHaveBeenCalled()
    expect(close).not.toHaveBeenCalled()
  })

  it('ignores an unrecognised outcome value', () => {
    const { postMessage } = stubWindow({ search: '?oauth=maybe', opener: true })

    expect(relayOAuthResultToOpener()).toBe(false)
    expect(postMessage).not.toHaveBeenCalled()
  })
})

describe('oauth listener', () => {
  it('accepts only same-origin messages carrying the agreed source', () => {
    const seen: unknown[] = []
    const stop = onOAuthResult(result => seen.push(result))

    const post = (data: unknown, origin: string) => window.dispatchEvent(new MessageEvent('message', { data, origin }))
    post({ source: 'nexusmail-oauth', status: 'success' }, 'https://evil.example.com')
    post({ source: 'something-else', status: 'success' }, location.origin)
    post({ status: 'success' }, location.origin)
    post(null, location.origin)
    post('nexusmail-oauth', location.origin)
    post({ source: 'nexusmail-oauth', status: 'unknown' }, location.origin)
    expect(seen).toEqual([])

    post({ source: 'nexusmail-oauth', status: 'error', reason: 'exchange_failed' }, location.origin)
    post({ source: 'nexusmail-oauth', status: 'success' }, location.origin)
    expect(seen).toEqual([
      { status: 'error', reason: 'exchange_failed' },
      { status: 'success', reason: undefined },
    ])

    stop()
    post({ source: 'nexusmail-oauth', status: 'success' }, location.origin)
    expect(seen).toHaveLength(2)
  })
})
