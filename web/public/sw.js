// NexusMail service worker.
//
// It exists for exactly one reason: notification action buttons are only allowed
// on notifications shown through a ServiceWorkerRegistration. Passing `actions`
// to `new Notification()` throws a TypeError, so the "copy the code" button in
// the macOS notification centre is impossible without this file.
//
// The worker deliberately does not cache anything. Mail is private and the app is
// served from the same process that holds the database, so an offline cache would
// only add a way for stale mail to reappear.
//
// This file is served verbatim from web/public/ and never passes through Vite or
// tsc: keep it plain browser JavaScript.

// Set when a notification is actioned with no page open to copy into. The next
// page to start claims it. Lost if the worker is terminated first, in which case
// the code is still readable in the notification and in the message detail.
let pendingCode = ''

self.addEventListener('install', () => {
  // Take over immediately instead of waiting for every tab to close, otherwise a
  // long-lived tab keeps an older worker alive and its notifications lose the
  // action handling added here.
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim())
})

self.addEventListener('notificationclick', event => {
  const data = event.notification.data || {}
  const code = data.code || ''
  event.notification.close()
  // '' is a click on the notification body itself. Safari does not render actions
  // at all, so treating a plain click as "copy" is what makes the feature degrade
  // instead of disappear there.
  if (code === '' || (event.action !== 'copy' && event.action !== '')) return
  // navigator.clipboard does not exist in a worker scope, so the write has to
  // happen in a page. Focus one (or open one) and hand the code over.
  event.waitUntil(deliver(code))
})

self.addEventListener('message', event => {
  // A page that has just started asks for anything the user actioned while no
  // page was open.
  if (event.data && event.data.type === 'CLAIM_OTP' && pendingCode) {
    event.source.postMessage({ type: 'COPY_OTP', code: pendingCode })
    pendingCode = ''
  }
})

async function deliver(code) {
  const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
  const target = windows.find(client => client.visibilityState === 'visible') || windows[0]
  if (target) {
    try { await target.focus() } catch { /* focus is best-effort */ }
    target.postMessage({ type: 'COPY_OTP', code: code })
    return
  }
  // No tab is open. openWindow resolves before the page has finished loading, so
  // it cannot receive a postMessage yet, and the code must not be put in the URL
  // where it would land in browser history. Park it for the page to claim.
  pendingCode = code
  await self.clients.openWindow('/')
}
