// The OAuth handshake used to be a full-page navigation: the SPA replaced itself
// with the provider's consent screen and came back on `/?oauth=success`, which
// threw away the open mailbox, the selected message and the scroll position, and
// silently dropped `?oauth=error&reason=…` because nothing ever read it.
//
// The consent screen now runs in a popup. The popup is on our origin again once
// the provider redirects to the callback, so it can hand the outcome to the
// window that opened it and close itself. Both halves live here rather than in
// main.tsx and AccountDialog.tsx so they can be tested without a module's
// import-time side effect.
//
// No COOP header is set on the SPA, so `window.opener` survives the round trip
// through the provider. `postMessage` is pinned to `location.origin` in both
// directions: the popup addresses only its own origin, and the listener drops
// anything that did not come from it, so a message from the consent screen — or
// from any other page that happens to hold a handle on this window — cannot
// impersonate a completed authorization.

export type OAuthResult = { status: 'success' | 'error'; reason?: string }

const messageSource = 'nexusmail-oauth'

// Called before React mounts. In the popup this is the whole program: read the
// outcome, hand it over, close. Returning true tells the caller not to render —
// mounting the app would fire off a mailbox load in a window that is about to
// disappear, and in the error case that window has no session cookie to use
// anyway, since the provider's redirect is a cross-site navigation.
export function relayOAuthResultToOpener(): boolean {
  const params = new URLSearchParams(location.search)
  const status = params.get('oauth')
  if (!window.opener || (status !== 'success' && status !== 'error')) return false
  const result: OAuthResult = { status }
  const reason = params.get('reason')
  if (reason) result.reason = reason
  window.opener.postMessage({ source: messageSource, ...result }, location.origin)
  window.close()
  return true
}

export function onOAuthResult(listener: (result: OAuthResult) => void): () => void {
  const handler = (event: MessageEvent) => {
    if (event.origin !== location.origin) return
    const data = event.data as { source?: unknown; status?: unknown; reason?: unknown } | null
    if (!data || data.source !== messageSource) return
    if (data.status !== 'success' && data.status !== 'error') return
    listener({ status: data.status, reason: typeof data.reason === 'string' ? data.reason : undefined })
  }
  window.addEventListener('message', handler)
  return () => window.removeEventListener('message', handler)
}
