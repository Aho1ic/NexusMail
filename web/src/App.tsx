import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { APIError, api, isAuthenticated } from './lib/api'
import { listenForCopyRequests, registerServiceWorker, showOTPNotification } from './lib/notifications'
import { loadPreferences, savePreferences, type Preferences } from './lib/preferences'
import { messageOf } from './lib/format'
import { notificationsEnabled, notify, useRealtime } from './hooks/useRealtime'
import { useKeyboard } from './hooks/useKeyboard'
import { AccountDialog } from './components/AccountDialog'
import { Composer } from './components/Composer'
import { Login } from './components/Login'
import { MailboxNav } from './components/MailboxNav'
import { MessageDetail } from './components/MessageDetail'
import { MessageList } from './components/MessageList'
import { OutboxDialog } from './components/OutboxDialog'
import { SettingsDialog } from './components/SettingsDialog'
import { Welcome } from './components/shared'
import type { Account, Draft, EventEnvelope, Mailbox, Message, MessageDetails } from './types'

type Pane = 'nav' | 'list' | 'detail'

export default function App() {
  const [authenticated, setAuthenticated] = useState(isAuthenticated())
  if (!authenticated) return <Login onAuthenticated={() => setAuthenticated(true)} />
  return <MailboxApp onLogout={() => setAuthenticated(false)} />
}

function MailboxApp({ onLogout }: { onLogout: () => void }) {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [mailboxes, setMailboxes] = useState<Mailbox[]>([])
  const [selectedAccount, setSelectedAccount] = useState<number | null>(null)
  const [selectedMailbox, setSelectedMailbox] = useState<number | null>(null)
  const [messages, setMessages] = useState<Message[]>([])
  const [selected, setSelected] = useState<Message | null>(null)
  const [details, setDetails] = useState<MessageDetails | null>(null)
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [cursor, setCursor] = useState<string | undefined>()
  const [unreadTotal, setUnreadTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [pane, setPane] = useState<Pane>('list')
  const [showAccounts, setShowAccounts] = useState(false)
  const [showComposer, setShowComposer] = useState(false)
  const [composerDraft, setComposerDraft] = useState<Draft | null>(null)
  const [showOutbox, setShowOutbox] = useState(false)
  const [showSettings, setShowSettings] = useState(false)
  const [preferences, setPreferences] = useState<Preferences>(loadPreferences)
  const [toast, setToast] = useState('')
  const [markingRead, setMarkingRead] = useState(false)

  // notify() runs from the socket handler outside React, so the live value is
  // mirrored into a module ref instead of being read from state.
  useEffect(() => { notificationsEnabled.current = preferences.desktopNotifications }, [preferences.desktopNotifications])

  // The toast is the only feedback for a copy that was triggered from the
  // notification centre, where nothing else on screen changes.
  const announce = useCallback((text: string) => {
    setToast(text)
    window.setTimeout(() => setToast(current => (current === text ? '' : current)), 2600)
  }, [])

  useEffect(() => {
    void registerServiceWorker()
    // The worker cannot reach the clipboard, so it forwards the code here after
    // the notification button is pressed.
    return listenForCopyRequests(({ code, copied }) => announce(copied ? `已复制验证码 ${code}` : `复制失败，验证码为 ${code}`))
  }, [announce])

  const updatePreferences = useCallback((patch: Partial<Preferences>) => {
    setPreferences(current => { const next = { ...current, ...patch }; savePreferences(next); return next })
  }, [])

  const compose = useCallback((draft: Draft | null = null) => { setComposerDraft(draft); setShowComposer(true) }, [])

  // One definition of "the current view", shared by the feed and by mark-all-read
  // so the button can never act on a different set than the list shows — including
  // the search term, which hides mail the button must not touch.
  const viewParams = useCallback(() => {
    const params = new URLSearchParams()
    if (selectedAccount) params.set('account_id', String(selectedAccount))
    if (selectedMailbox) params.set('mailbox_id', String(selectedMailbox)); else params.set('folder', 'inbox')
    if (debouncedQuery) params.set('query', debouncedQuery)
    return params
  }, [selectedAccount, selectedMailbox, debouncedQuery])

  const loadAccounts = useCallback(async () => {
    try { const result = await api.accounts(); setAccounts(result.items) }
    catch (err) { if (err instanceof APIError && err.status === 401) onLogout(); else setError(messageOf(err)) }
  }, [onLogout])

  const loadMailboxes = useCallback(async (accountID: number | null) => {
    if (!accountID) { setMailboxes([]); return }
    try { setMailboxes((await api.mailboxes(accountID)).items) } catch (err) { setError(messageOf(err)) }
  }, [])

  const loadMessages = useCallback(async (append = false, nextCursor?: string, quiet = false) => {
    if (!quiet) setLoading(true)
    setError('')
    const params = viewParams()
    params.set('limit', '40')
    if (nextCursor) params.set('cursor', nextCursor)
    try {
      const page = await api.messages(params)
      setMessages(current => append ? [...current, ...page.items.filter(item => !current.some(existing => existing.id === item.id))] : page.items)
      setCursor(page.next_cursor)
      setUnreadTotal(page.unread_total ?? 0)
    } catch (err) { setError(messageOf(err)) } finally { setLoading(false) }
  }, [viewParams])

  useEffect(() => { loadAccounts() }, [loadAccounts])
  useEffect(() => { loadMailboxes(selectedAccount); setSelectedMailbox(null) }, [selectedAccount, loadMailboxes])
  useEffect(() => { const timer = window.setTimeout(() => setDebouncedQuery(query.trim()), 250); return () => clearTimeout(timer) }, [query])
  useEffect(() => { loadMessages() }, [loadMessages])

  const refresh = useCallback(() => { loadAccounts(); loadMailboxes(selectedAccount); loadMessages() }, [loadAccounts, loadMailboxes, loadMessages, selectedAccount])
  // Realtime arrivals only need the message list — account list and mailbox
  // structure do not change on NEW_EMAIL / MESSAGE_UPDATED, so calling them
  // here only adds an extra REST round-trip to the hot path.
  const refreshQuietly = useCallback(() => { loadMessages(false, undefined, true) }, [loadMessages])

  // Keyed by message and code, not by message alone: the arrival pass only sees
  // the subject, so a later body pass may carry a better code that should still
  // reach the user, while a repeat of the same code must not notify twice.
  const notifiedCodes = useRef(new Set<string>())
  const handleEvent = useCallback((payload: EventEnvelope) => {
    const code = typeof payload.data?.otp_code === 'string' ? payload.data.otp_code : ''
    // The code notification replaces the generic notice rather than adding to it,
    // so switching only the code notification off has to fall back to the generic
    // one instead of leaving the arrival silent.
    if (code && preferences.desktopNotifications && preferences.verificationCodeNotifications) {
      const messageID = typeof payload.data?.message_id === 'number' ? payload.data.message_id : 0
      const key = `${messageID}:${code}`
      if (notifiedCodes.current.has(key)) return
      notifiedCodes.current.add(key)
      const subject = typeof payload.data?.otp_subject === 'string' ? payload.data.otp_subject : ''
      void showOTPNotification(code, subject, messageID)
      return
    }
    if (payload.type === 'NEW_EMAIL') notify()
  }, [preferences.desktopNotifications, preferences.verificationCodeNotifications])
  useRealtime(refreshQuietly, handleEvent)

  async function markViewRead() {
    setMarkingRead(true)
    try {
      const result = await api.markAllRead(viewParams())
      // A full refresh, not a local map: the unread badges are derived from the
      // loaded page, so a partial update would leave stale counts behind.
      refresh()
      if (result.updated === 0) announce('当前视图没有未读邮件')
      else if (result.partial) announce(`已标记 ${result.updated} 封，部分账户同步失败`)
      else if (result.capped) announce(`已标记 ${result.updated} 封，仍有未读邮件，可再次点击`)
      else announce(`已标记 ${result.updated} 封为已读`)
    } catch (err) { setError(messageOf(err)) } finally { setMarkingRead(false) }
  }

  async function openMessage(message: Message) {
    setSelected(message); setPane('detail'); setDetails(null)
    if (!message.is_read) {
      setMessages(items => items.map(item => item.id === message.id ? { ...item, is_read: true } : item))
      // The badge tracks the server total, so opening a message has to draw it down
      // here as well; otherwise the count only moves on the next feed load.
      setUnreadTotal(total => Math.max(total - 1, 0))
      api.patchMessage(message.id, { is_read: true }).catch(() => undefined)
    }
    try { setDetails(await api.message(message.id)) } catch (err) { setError(messageOf(err)) }
  }

  async function mutateMessage(patch: object) {
    if (!selected) return
    try {
      const updated = await api.patchMessage(selected.id, patch)
      setSelected(updated); setMessages(items => items.map(item => item.id === updated.id ? updated : item))
      if ('archive' in patch) { setMessages(items => items.filter(item => item.id !== selected.id)); setSelected(null); setDetails(null); setPane('list') }
    } catch (err) { setError(messageOf(err)) }
  }

  useKeyboard(preferences.keyboardShortcuts, messages, selected, openMessage, () => compose(), () => mutateMessage({ archive: true }))

  async function logout() { try { await api.logout() } finally { onLogout() } }
  const accountMap = useMemo(() => new Map(accounts.map(account => [account.id, account])), [accounts])
  // The server counts the whole view; the loaded page only holds 40 rows, so
  // counting locally reported "0 unread" on any view whose unread mail sits past
  // the first page and left mark-all-read disabled with work still to do. The page
  // count is the floor for the case where a local read has not been counted yet.
  const unreadCount = useMemo(() => Math.max(unreadTotal, messages.filter(item => !item.is_read).length), [unreadTotal, messages])
  const listTitle = selectedMailbox
    ? mailboxes.find(box => box.id === selectedMailbox)?.display_name
    : selectedAccount ? accountMap.get(selectedAccount)?.display_name || accountMap.get(selectedAccount)?.email : 'All Inboxes'

  return <div className="h-screen bg-paper text-ink p-0 md:p-3 lg:p-5 overflow-hidden">
    <div className="mx-auto flex h-full max-w-[1680px] overflow-hidden bg-white md:rounded-[1.8rem] md:border md:border-black/5 md:shadow-panel">
      <MailboxNav visible={pane === 'nav'} accounts={accounts} mailboxes={mailboxes} selectedAccount={selectedAccount} selectedMailbox={selectedMailbox} unreadCount={unreadCount}
        onCompose={() => compose()}
        onSelectAll={() => { setSelectedAccount(null); setSelectedMailbox(null); setPane('list') }}
        onSelectAccount={id => { setSelectedAccount(id); setSelectedMailbox(null); setPane('list') }}
        onSelectMailbox={id => { setSelectedMailbox(id); setPane('list') }}
        onShowOutbox={() => setShowOutbox(true)} onShowAccounts={() => setShowAccounts(true)} onShowSettings={() => setShowSettings(true)} onLogout={logout} />

      <MessageList visible={pane === 'list'} title={listTitle} messages={messages} accountMap={accountMap} selected={selected} unreadCount={unreadCount}
        markingRead={markingRead} loading={loading} error={error} query={query} cursor={cursor}
        onOpenNav={() => setPane('nav')} onMarkViewRead={markViewRead} onRefresh={refresh} onQueryChange={setQuery}
        onOpen={openMessage} onLoadMore={() => loadMessages(true, cursor)} />

      <main className={`${pane === 'detail' ? 'flex' : 'hidden'} md:flex min-w-0 flex-1 flex-col bg-white`}>
        {selected ? <MessageDetail selected={selected} details={details} autoLoadRemoteImages={preferences.autoLoadRemoteImages} onBack={() => setPane('list')} onStar={() => mutateMessage({ is_starred: !selected.is_starred })} onArchive={() => mutateMessage({ archive: true })} onReply={() => compose()} onNotice={announce} /> : <Welcome count={unreadCount} />}
      </main>
    </div>
    {showAccounts && <AccountDialog onClose={() => setShowAccounts(false)} onCreated={() => { setShowAccounts(false); loadAccounts() }} />}
    {showOutbox && <OutboxDialog onClose={() => setShowOutbox(false)} onEdit={draft => { setShowOutbox(false); compose(draft) }} />}
    {showComposer && <Composer accounts={accounts} replyTo={composerDraft ? null : selected} initialDraft={composerDraft} onClose={() => setShowComposer(false)} onSent={() => { setShowComposer(false); refresh() }} />}
    {showSettings && <SettingsDialog preferences={preferences} accounts={accounts} onChange={updatePreferences} onClose={() => setShowSettings(false)} onAddAccount={() => { setShowSettings(false); setShowAccounts(true) }} onLogout={logout} />}
    {toast && <div role="status" className="pointer-events-none fixed bottom-6 left-1/2 z-50 -translate-x-1/2 rounded-full bg-pine px-5 py-2.5 text-xs font-semibold text-white shadow-panel">{toast}</div>}
  </div>
}
