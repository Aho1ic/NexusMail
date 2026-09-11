import type { Account, Attachment, Draft, DraftInput, Mailbox, MarkReadResult, Message, MessageDetails, MessagePage } from '../types'

const csrfKey = 'nexusmail.csrf'
const expiryKey = 'nexusmail.session-expires'
const sessionPath = '/api/v1/auth/session'
const sessionInvalidatedEvent = 'nexusmail:session-invalidated'

// The session cookie carries a fixed 12-hour Expires and is never renewed, so the
// browser drops it at a deadline the server told us at login. The CSRF token has no
// such deadline of its own: kept in localStorage it outlived the cookie, so
// `isAuthenticated()` still said yes long after the cookie was gone and the app
// mounted the mailbox instead of the login screen. The first mutation then reached
// the server with no cookie at all and came back 401 `authentication required`,
// which the user saw as "标记已读失败". Storing the deadline alongside the token is
// what makes the two expire together.
function sessionExpired() {
  const deadline = Number(localStorage.getItem(expiryKey) ?? sessionStorage.getItem(expiryKey) ?? 0)
  // A token stored before the deadline was recorded has none to check against, and
  // must stay usable: its 401, if it is past due, is what clears it.
  return deadline > 0 && Date.now() >= deadline
}

function csrfToken() {
  // Cookies are shared across tabs. Prefer the token from the latest login, with
  // a fallback for tabs opened before shared storage was introduced.
  return localStorage.getItem(csrfKey) ?? sessionStorage.getItem(csrfKey) ?? ''
}

function clearSession() {
  sessionStorage.removeItem(csrfKey)
  sessionStorage.removeItem(expiryKey)
  localStorage.removeItem(expiryKey)
  // An empty shared value prevents a legacy tab from reviving its old token.
  localStorage.setItem(csrfKey, '')
  window.dispatchEvent(new Event(sessionInvalidatedEvent))
}

// The invalidation signal is a window event, and App only subscribes from an effect
// after its first render. A 401 that lands in that window - the mount-time feed
// loads all race it - emptied the token with nobody listening, so the mailbox stayed
// on screen with no session and every later action answered with the server's raw
// `authentication required`. Reporting an already-cleared session at subscribe time
// makes the signal impossible to miss, whenever it happened to fire.
export function onSessionInvalidated(listener: () => void) {
  const onStorage = (event: StorageEvent) => {
    if (event.storageArea !== localStorage || (event.key !== csrfKey && event.key !== null)) return
    sessionStorage.removeItem(csrfKey)
    if (!localStorage.getItem(csrfKey)) listener()
  }
  window.addEventListener(sessionInvalidatedEvent, listener)
  window.addEventListener('storage', onStorage)
  if (!isAuthenticated()) listener()
  return () => {
    window.removeEventListener(sessionInvalidatedEvent, listener)
    window.removeEventListener('storage', onStorage)
  }
}

export class APIError extends Error {
  constructor(public status: number, public code: string, message: string) { super(message) }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  const method = (init.method ?? 'GET').toUpperCase()
  const isLogin = path === sessionPath && method === 'POST'
  const csrf = csrfToken()
  if (!isLogin && !['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    if (csrf) headers.set('X-CSRF-Token', csrf)
  }
  const response = await fetch(path, { ...init, headers, credentials: 'include' })
  // A response from before a new login must not invalidate the new session.
  if (response.status === 401 && !isLogin && csrf === csrfToken()) clearSession()
  const contentType = response.headers.get('content-type') ?? ''
  const payload = contentType.includes('json') ? await response.json() : null
  if (!response.ok && response.status !== 202) {
    const error = payload?.error
    throw new APIError(response.status, error?.code ?? 'request_failed', error?.message ?? `HTTP ${response.status}`)
  }
  return payload as T
}

export const api = {
  async login(apiKey: string) {
    const result = await request<{ csrf_token: string; expires_at: number }>(sessionPath, { method: 'POST', body: JSON.stringify({ api_key: apiKey }) })
    localStorage.setItem(csrfKey, result.csrf_token)
    // The server already reports when the cookie it just set stops being sent.
    // Keeping it is what lets a returning tab tell a live session from a lapsed one
    // before it makes a request that cannot succeed.
    if (result.expires_at) localStorage.setItem(expiryKey, String(result.expires_at))
    else localStorage.removeItem(expiryKey)
    sessionStorage.removeItem(csrfKey)
    sessionStorage.removeItem(expiryKey)
  },
  async logout() {
    const csrf = csrfToken()
    try { await request(sessionPath, { method: 'DELETE' }) }
    finally { if (csrf === csrfToken()) clearSession() }
  },
  accounts: () => request<{ items: Account[] }>('/api/v1/accounts'),
  addAccount: (input: unknown) => request<Account | { authorization_url: string }>('/api/v1/accounts', { method: 'POST', body: JSON.stringify(input) }),
  deleteAccount: (id: number) => request(`/api/v1/accounts/${id}`, { method: 'DELETE' }),
  mailboxes: (accountID: number) => request<{ items: Mailbox[] }>(`/api/v1/accounts/${accountID}/mailboxes`),
  messages: (params: URLSearchParams) => request<MessagePage>(`/api/v1/messages?${params}`),
  markAllRead: (params: URLSearchParams) => request<MarkReadResult>(`/api/v1/messages/mark-read?${params}`, { method: 'POST' }),
  message: (id: number) => request<MessageDetails>(`/api/v1/messages/${id}`),
  patchMessage: (id: number, patch: object) => request<Message>(`/api/v1/messages/${id}`, { method: 'PATCH', body: JSON.stringify(patch) }),
  drafts: (status = '') => request<{ items: Draft[] }>(`/api/v1/drafts${status ? `?status=${status}` : ''}`),
  draft: (id: number) => request<{ draft: Draft; attachments: Attachment[] }>(`/api/v1/drafts/${id}`),
  createDraft: (input: DraftInput) => request<Draft>('/api/v1/drafts', { method: 'POST', body: JSON.stringify(input) }),
  updateDraft: (id: number, revision: number, input: DraftInput) => request<Draft>(`/api/v1/drafts/${id}`, { method: 'PATCH', headers: { 'If-Match': String(revision) }, body: JSON.stringify(input) }),
  deleteDraft: (id: number) => request(`/api/v1/drafts/${id}`, { method: 'DELETE' }),
  uploadAttachment: (id: number, file: File) => { const body = new FormData(); body.set('file', file); return request(`/api/v1/drafts/${id}/attachments`, { method: 'POST', body }) },
  sendDraft: (id: number) => request(`/api/v1/drafts/${id}/send`, { method: 'POST' }),
  retryDraft: (id: number) => request(`/api/v1/drafts/${id}/retry`, { method: 'POST' }),
}

export function isAuthenticated() { return Boolean(csrfToken()) && !sessionExpired() }
