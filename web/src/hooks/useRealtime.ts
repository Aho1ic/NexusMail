import { useEffect, useRef } from 'react'
import { loadPreferences } from '../lib/preferences'
import type { EventEnvelope } from '../types'

const realtimeEvents = ['NEW_EMAIL', 'MESSAGE_UPDATED', 'ACCOUNT_STATUS', 'OUTBOX_UPDATED']

// The socket handler calls notify() from outside the React tree, so the desktop
// notification preference is mirrored here rather than read from state.
export const notificationsEnabled = { current: loadPreferences().desktopNotifications }

// Notification is absent in insecure contexts and some browsers; a throw here
// would kill the socket handler and stall realtime updates.
export function notify() {
  if (!notificationsEnabled.current) return
  try { if ('Notification' in window && Notification.permission === 'granted') new Notification('NexusMail 收到新邮件') }
  catch { /* notifications are best-effort */ }
}

export function useRealtime(onChange: () => void, onEvent: (payload: EventEnvelope) => void) {
  // Events are not persisted server-side, so the socket must outlive filter
  // changes. Holding the callbacks in refs keeps one connection for the
  // session instead of reconnecting whenever the mailbox or query changes.
  const handler = useRef(onChange)
  const events = useRef(onEvent)
  useEffect(() => { handler.current = onChange }, [onChange])
  useEffect(() => { events.current = onEvent }, [onEvent])
  useEffect(() => {
    let socket: WebSocket | undefined; let timer = 0; let coalesce = 0; let stopped = false; let delay = 250
    // A backlog of body fetches emits a burst of events; coalesce them into one refresh.
    const schedule = () => { window.clearTimeout(coalesce); coalesce = window.setTimeout(() => handler.current(), 80) }
    const connect = () => {
      const scheme = location.protocol === 'https:' ? 'wss' : 'ws'; socket = new WebSocket(`${scheme}://${location.host}/api/v1/ws`)
      // Any event missed while reconnecting is unrecoverable, so resync on open.
      socket.onopen = () => { delay = 250; schedule() }
      // The payload is forwarded verbatim: only the caller knows whether the event
      // carries a verification code worth notifying about.
      socket.onmessage = event => { const payload = JSON.parse(event.data) as EventEnvelope; if (realtimeEvents.includes(payload.type)) { schedule(); events.current(payload) } }
      socket.onclose = () => { if (!stopped) { timer = window.setTimeout(connect, delay); delay = Math.min(delay * 2, 10000) } }
    }
    connect()
    return () => { stopped = true; clearTimeout(timer); clearTimeout(coalesce); socket?.close() }
  }, [])
}
