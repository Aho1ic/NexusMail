import type { Account, Attachment, Draft, DraftInput, Mailbox, MarkReadResult, Message, MessageDetails, MessagePage } from '../types'

const csrfKey = 'nexusmail.csrf'

export class APIError extends Error {
  constructor(public status: number, public code: string, message: string) { super(message) }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  const method = (init.method ?? 'GET').toUpperCase()
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    const csrf = sessionStorage.getItem(csrfKey)
    if (csrf) headers.set('X-CSRF-Token', csrf)
  }
  const response = await fetch(path, { ...init, headers, credentials: 'include' })
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
    const result = await request<{ csrf_token: string }>('/api/v1/auth/session', { method: 'POST', body: JSON.stringify({ api_key: apiKey }) })
    sessionStorage.setItem(csrfKey, result.csrf_token)
  },
  async logout() { await request('/api/v1/auth/session', { method: 'DELETE' }); sessionStorage.removeItem(csrfKey) },
  accounts: () => request<{ items: Account[] }>('/api/v1/accounts'),
  addAccount: (input: unknown) => request<Account | { authorization_url: string }>('/api/v1/accounts', { method: 'POST', body: JSON.stringify(input) }),
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

export function isAuthenticated() { return Boolean(sessionStorage.getItem(csrfKey)) }
