// Verification-code notifications and clipboard access.
//
// The copy button in the notification is the whole point of registering a service
// worker: `actions` is only honoured by
// `ServiceWorkerRegistration.showNotification()`. Passing it to `new
// Notification()` throws a TypeError, so there is no page-only version of this.
// The worker cannot write the clipboard either — `navigator.clipboard` is absent
// from worker scope — so the click travels worker → page and the page copies.

export type CopyOutcome = { code: string; copied: boolean }

// TypeScript's DOM lib models neither `actions` nor `Notification.maxActions`,
// because both only mean anything for a service-worker notification. They are
// declared here instead of casting to any at each use site.
type NotificationActionOption = { action: string; title: string; icon?: string }
type OTPNotificationOptions = NotificationOptions & { actions?: NotificationActionOption[] }

let registration: ServiceWorkerRegistration | null = null

function supported() {
  // Service workers and the clipboard both require a secure context. localhost
  // counts; a plain-HTTP LAN address does not, and there the whole feature is
  // simply absent rather than broken.
  return typeof navigator !== 'undefined' && 'serviceWorker' in navigator && window.isSecureContext
}

export async function registerServiceWorker() {
  if (!supported()) return null
  try {
    registration = await navigator.serviceWorker.register('/sw.js')
    return registration
  } catch { return null }
}

// notificationsAllowed keeps every caller on the same guard: an unsupported
// browser, a revoked permission, or a throwing constructor must never take down
// the socket handler that calls into here.
function notificationsAllowed() {
  try { return 'Notification' in window && Notification.permission === 'granted' }
  catch { return false }
}

export async function showOTPNotification(code: string, subject: string, messageID: number) {
  if (!code || !notificationsAllowed()) return false
  const title = '收到验证码'
  const body = `${code}\n${subject || '（无主题）'}`
  const active = registration ?? (supported() ? await navigator.serviceWorker.getRegistration() : null)
  const options: OTPNotificationOptions = {
    body,
    // The same tag for both detection passes: the subject-only guess raised on
    // arrival is replaced by the body-derived one instead of stacking.
    tag: `otp-${messageID}`,
    icon: '/notification-icon-192.png',
    badge: '/notification-icon-192.png',
    // Verification codes are consumed within minutes, so the notification has to
    // wait in the notification centre rather than auto-dismissing.
    requireInteraction: true,
    data: { code, messageID },
    // maxActions is 0 where actions are unsupported; passing them anyway is
    // rejected in some engines.
    actions: maxActions() > 0 ? [{ action: 'copy', title: '复制验证码' }] : undefined,
  }
  if (active) {
    try {
      await active.showNotification(title, options)
      return true
    } catch { /* fall through to the plain notification */ }
  }
  // No worker: still tell the user, just without the button. The code is in the
  // body, and the message detail carries a copy control.
  try { new Notification(title, { body, tag: `otp-${messageID}` }); return true }
  catch { return false }
}

function maxActions() {
  try {
    const limit = (Notification as unknown as { maxActions?: number }).maxActions
    return typeof limit === 'number' ? limit : 0
  } catch { return 0 }
}

export async function copyText(value: string) {
  if (!value) return false
  try {
    await navigator.clipboard.writeText(value)
    return true
  } catch { /* denied, insecure context, or no clipboard API */ }
  // execCommand is deprecated but remains the only fallback when the async
  // clipboard is unavailable, which is exactly when the user needs one.
  //
  // The field is removed in a finally rather than after the copy: on a browser
  // where execCommand is absent or throws instead of returning false, an
  // after-the-copy removal never runs and the hidden textarea stays in the
  // document, one per failed attempt. remove() is a no-op if it was never appended.
  const field = document.createElement('textarea')
  try {
    field.value = value
    field.setAttribute('readonly', '')
    field.style.position = 'fixed'
    field.style.opacity = '0'
    document.body.appendChild(field)
    field.select()
    return document.execCommand('copy')
  } catch { return false }
  finally { field.remove() }
}

// listenForCopyRequests wires the notification button to the clipboard: the
// worker forwards the code here after focusing the page. It also claims a code
// the user actioned while no tab was open, which the worker parked rather than
// putting into the URL where it would land in browser history.
export function listenForCopyRequests(onCopy: (outcome: CopyOutcome) => void) {
  if (!supported()) return () => undefined
  const handler = (event: MessageEvent) => {
    const data = event.data as { type?: string; code?: string } | null
    if (!data || data.type !== 'COPY_OTP' || !data.code) return
    const code = data.code
    void copyText(code).then(copied => onCopy({ code, copied }))
  }
  navigator.serviceWorker.addEventListener('message', handler)
  void navigator.serviceWorker.ready
    .then(ready => ready.active?.postMessage({ type: 'CLAIM_OTP' }))
    .catch(() => undefined)
  return () => navigator.serviceWorker.removeEventListener('message', handler)
}
