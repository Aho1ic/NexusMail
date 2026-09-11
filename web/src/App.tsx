import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { APIError, api, isAuthenticated, onSessionInvalidated } from './lib/api'
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
  const [authenticated, setAuthenticated] = useState(isAuthenticated)
  useEffect(() => onSessionInvalidated(() => setAuthenticated(false)), [])
  if (!authenticated) return <Login onAuthenticated={() => setAuthenticated(true)} />
  return <MailboxApp onLogout={() => setAuthenticated(false)} />
}

function MailboxApp({ onLogout }: { onLogout: () => void }) {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [mailboxes, setMailboxes] = useState<Mailbox[]>([])
  const [selectedAccount, setSelectedAccount] = useState<number | null>(null)
  const [selectedMailbox, setSelectedMailbox] = useState<number | null>(null)
  const [foldersCollapsed, setFoldersCollapsed] = useState(false)
  const [messages, setMessages] = useState<Message[]>([])
  const [selected, setSelected] = useState<Message | null>(null)
  const [details, setDetails] = useState<MessageDetails | null>(null)
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [cursor, setCursor] = useState<string | undefined>()
  const [unreadTotal, setUnreadTotal] = useState(0)
  const [revealID, setRevealID] = useState<number | null>(null)
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
    catch (err) { setError(messageOf(err)) }
  }, [])

  const loadMailboxes = useCallback(async (accountID: number | null) => {
    if (!accountID) { setMailboxes([]); return }
    try { setMailboxes((await api.mailboxes(accountID)).items) } catch (err) { setError(messageOf(err)) }
  }, [])

  // Set by the All Inboxes press, consumed by the next non-append load. A ref, not
  // state: the effect-driven load must see it in the same tick it was set, and a
  // state update would arrive one render too late.
  const pendingReveal = useRef(false)

  // Every load that writes into state carries a generation. The guard exists
  // because the abandoned view's response can arrive after the new one: switching
  // account or mailbox, or opening a second message, leaves the first request in
  // flight, and without this the later-resolving older response wins and renders a
  // view nobody is looking at. Bumped on entry, compared before every write.
  const generation = useRef(0)

  const loadMessages = useCallback(async (append = false, nextCursor?: string, quiet = false) => {
    const current = ++generation.current
    if (!quiet) setLoading(true)
    setError('')
    const params = viewParams()
    // The reveal load asks for the server's maximum page. The unread mail the badge
    // counts is regularly past row 40 — the reported case sat at row 69 — so a
    // default page could not contain the row it was sent to find.
    const revealing = !append && pendingReveal.current
    pendingReveal.current = false
    params.set('limit', revealing ? '100' : '40')
    if (nextCursor) params.set('cursor', nextCursor)
    try {
      const page = await api.messages(params)
      if (current !== generation.current) return
      setMessages(items => append ? [...items, ...page.items.filter(item => !items.some(existing => existing.id === item.id))] : page.items)
      setCursor(page.next_cursor)
      setUnreadTotal(page.unread_total ?? 0)
      if (revealing) {
        // Same predicate as the badge's server-side count, so the row picked here is
        // one the badge is actually counting.
        const target = page.items.find(item => !item.is_read && item.direction === 'incoming')
        setRevealID(target?.id ?? null)
        if (!target && (page.unread_total ?? 0) > 0) announce('未读邮件不在最近 100 封内，可继续向下加载')
      }
    } catch (err) { if (current === generation.current) setError(messageOf(err)) }
    // The spinner belongs to the newest load, so a superseded one must leave it up.
    finally { if (current === generation.current) setLoading(false) }
  }, [viewParams, announce])

  useEffect(() => { loadAccounts() }, [loadAccounts])
  // Only the folder list is loaded here. Clearing the selected mailbox belongs to
  // the two handlers that change the account, which do it in the same render — an
  // effect would clear it one render late and spend a feed request on a mailbox the
  // new account cannot see.
  useEffect(() => { loadMailboxes(selectedAccount) }, [selectedAccount, loadMailboxes])
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
  // The feed refresh cannot reach the open message: MessageDetail renders the
  // snapshot taken when it was opened, so a body that arrives afterwards left the
  // pane on "will refresh automatically" until the message was reopened. Only an
  // event naming this message re-fetches — the body-prefetch backlog emits these
  // in bursts, and the rest of them concern mail that is not on screen.
  const selectedID = selected?.id
  // Counted separately from the feed: a quiet feed refresh must not discard the
  // details of the message still on screen, and opening another message must
  // discard the previous one's details.
  const detailGeneration = useRef(0)
  const refreshOpenMessage = useCallback(async (messageID: number) => {
    if (messageID !== selectedID) return
    const current = detailGeneration.current
    try {
      const loaded = await api.message(messageID)
      // Same reason as the feed guard: this response can land after the user has
      // already opened something else.
      if (current === detailGeneration.current) setDetails(loaded)
    } catch { /* the next event or a reopen retries */ }
  }, [selectedID])

  const handleEvent = useCallback((payload: EventEnvelope) => {
    const updatedID = typeof payload.data?.message_id === 'number' ? payload.data.message_id : 0
    if (payload.type === 'MESSAGE_UPDATED' && updatedID) void refreshOpenMessage(updatedID)
    // The account list is the only carrier of status and last_error, so the
    // connection dot, the sync-failure banner and the settings lines cannot move
    // without re-reading it. refreshQuietly deliberately skips it, which left the
    // one event that exists to report a status change unable to show one. Kept off
    // the NEW_EMAIL hot path by keying on this type alone.
    if (payload.type === 'ACCOUNT_STATUS') void loadAccounts()
    const code = typeof payload.data?.otp_code === 'string' ? payload.data.otp_code : ''
    // The code notification replaces the generic notice rather than adding to it,
    // so switching only the code notification off has to fall back to the generic
    // one instead of leaving the arrival silent.
    if (code && preferences.desktopNotifications && preferences.verificationCodeNotifications) {
      const key = `${updatedID}:${code}`
      if (notifiedCodes.current.has(key)) return
      notifiedCodes.current.add(key)
      const subject = typeof payload.data?.otp_subject === 'string' ? payload.data.otp_subject : ''
      void showOTPNotification(code, subject, updatedID)
      return
    }
    if (payload.type === 'NEW_EMAIL') notify()
  }, [loadAccounts, preferences.desktopNotifications, preferences.verificationCodeNotifications, refreshOpenMessage])
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
    // The highlight has served its purpose once the row is opened; leaving it on
    // would keep ringing a row the user is already reading.
    setSelected(message); setPane('detail'); setDetails(null); setRevealID(null)
    if (!message.is_read) {
      setMessages(items => items.map(item => item.id === message.id ? { ...item, is_read: true } : item))
      // The badge tracks the server total, so opening a message has to draw it down
      // here as well; otherwise the count only moves on the next feed load.
      setUnreadTotal(total => Math.max(total - 1, 0))
      // The failure is rolled back rather than swallowed. The row is drawn read
      // before the server has agreed, so a discarded error left the UI claiming
      // read while the message was still unread everywhere else — and the user only
      // found out on the next feed load, which is what "it went unread again after
      // refreshing" looks like. Undoing it puts the truth back on screen at once.
      api.patchMessage(message.id, { is_read: true }).catch(err => {
        setMessages(items => items.map(item => item.id === message.id ? { ...item, is_read: false } : item))
        setUnreadTotal(total => total + 1)
        // A 401 is not a mark-read failure. The session lapsed, the transport has
        // already cleared it and the app is on its way to the login screen, so
        // blaming this action - in the server's own English, at that - described the
        // wrong problem to the one user who cannot act on it.
        if (err instanceof APIError && err.status === 401) return
        announce(`标记已读失败：${messageOf(err)}`)
      })
    }
    const current = ++detailGeneration.current
    try {
      const loaded = await api.message(message.id)
      // Clicking A then B resolves last-response-wins without this: A's response can
      // land after B's and would render A's subject, sender and body under B's
      // highlighted row.
      if (current !== detailGeneration.current) return
      setDetails(loaded)
    } catch (err) { if (current === detailGeneration.current) setError(messageOf(err)) }
  }

  async function mutateMessage(patch: object) {
    if (!selected) return
    try {
      const updated = await api.patchMessage(selected.id, patch)
      setSelected(updated); setMessages(items => items.map(item => item.id === updated.id ? updated : item))
      if ('archive' in patch) { setMessages(items => items.filter(item => item.id !== selected.id)); setSelected(null); setDetails(null); setPane('list') }
    } catch (err) { setError(messageOf(err)) }
  }

  // The shortcuts are bound to window and every dialog renders as a sibling of the
  // still-mounted mailbox, so without this 'e' archived the message behind an open
  // dialog — destructive, and not undoable from this UI — and 'c' mounted a second
  // composer over the first.
  const dialogOpen = showAccounts || showComposer || showOutbox || showSettings
  useKeyboard(preferences.keyboardShortcuts, dialogOpen, messages, selected, openMessage, () => compose(), () => mutateMessage({ archive: true }))

  // The local session ends either way. Swallowing the failure rather than letting
  // `finally` re-throw keeps a rejected DELETE from escaping the click handler as
  // an unhandled rejection, which is all the caller would ever see of it.
  async function logout() { try { await api.logout() } catch { /* the session is over locally regardless */ } finally { onLogout() } }

  // The deleted account may be the one on screen, and its mailboxes and mail went
  // with it. Releasing the scope is enough to reload both: the folder effect and the
  // feed effect already key off it. refresh() cannot be used here — it would read
  // the pre-update selectedAccount and spend a folder request on an account the
  // server has just forgotten.
  function forgetAccount(id: number) {
    loadAccounts()
    if (selectedAccount !== id) { loadMessages(); return }
    setSelectedAccount(null); setSelectedMailbox(null); setSelected(null); setDetails(null); setPane('list')
  }
  const accountMap = useMemo(() => new Map(accounts.map(account => [account.id, account])), [accounts])
  // The server counts the whole view; the loaded page only holds 40 rows, so
  // counting locally reported "0 unread" on any view whose unread mail sits past
  // the first page and left mark-all-read disabled with work still to do. The page
  // count is the floor for the case where a local read has not been counted yet.
  const unreadCount = useMemo(() => Math.max(unreadTotal, messages.filter(item => !item.is_read).length), [unreadTotal, messages])
  const listTitle = selectedMailbox
    ? mailboxes.find(box => box.id === selectedMailbox)?.display_name
    : selectedAccount ? accountMap.get(selectedAccount)?.display_name || accountMap.get(selectedAccount)?.email : 'All Inboxes'

  // Pressing All Inboxes when the badge shows unread mail is a request to be taken
  // to that mail, not just to the top of the list. Leaving another view changes
  // viewParams and the existing effect issues the load, so the flag alone redirects
  // it; standing on All Inboxes already changes nothing, so the load is explicit.
  function selectAll() {
    const already = selectedAccount === null && selectedMailbox === null
    setSelectedAccount(null)
    setSelectedMailbox(null)
    setPane('list')
    if (already && unreadCount === 0) return
    pendingReveal.current = true
    if (already) void loadMessages()
  }

  // Pressing the account row toggles its folder tree. The collapse branch is only
  // reachable once the row already stands for the current view — account selected,
  // no folder under it — so the first press out of a folder still does what it always
  // did and releases the mailbox scope. Collapsing leaves the scope alone: it hides
  // the folder list, it does not change which mail is listed, so no feed request.
  // The pane is left alone too, or on a narrow screen the collapse would be hidden
  // behind the feed the moment it happened.
  function selectAccount(id: number) {
    if (selectedAccount === id && !selectedMailbox) { setFoldersCollapsed(current => !current); return }
    setSelectedAccount(id)
    setSelectedMailbox(null)
    setFoldersCollapsed(false)
    setPane('list')
  }

  // Three stacked layers: the lit stage, the frosted shell floating on it, and the
  // panes floating inside the shell. The orbs sit behind the shell and are decorative
  // only — they are hidden below md, where the shell is full-bleed and no gutter shows.
  // The gutter is deliberately thin and does not grow with the viewport: it exists
  // only to give the shell's radius and shadow somewhere to land. At 8px the shell is
  // within 0.5% of the full viewport, and dropping to 0 would buy that back at the
  // cost of clipping the shadow against the screen edge.
  return <div className="app-stage relative isolate h-screen overflow-hidden p-0 text-ink md:p-2">
    <div aria-hidden className="pointer-events-none absolute -left-16 -top-24 hidden h-[26rem] w-[26rem] rounded-full bg-sage/70 blur-[110px] md:block" />
    <div aria-hidden className="pointer-events-none absolute -bottom-28 -right-20 hidden h-[30rem] w-[30rem] rounded-full bg-coral/20 blur-[120px] md:block" />
    <div aria-hidden className="pointer-events-none absolute -top-32 right-1/4 hidden h-[22rem] w-[22rem] rounded-full bg-pine/10 blur-[130px] lg:block" />
    {/* No width cap: the shell fills the viewport. The old 1680px cap was the real
        source of the empty margin — on a 2560px screen it left 440px unused on each
        side, against 36px from the padding. (Written without the bracket syntax on
        purpose: Tailwind scans comments too, and spelling the class out here made it
        emit the dead rule this change removes.) Wide screens are safe because the
        reading pane caps its own text at max-w-3xl, so the extra width becomes
        margin around the article rather than 200-character lines.
        The frosted frame is only visible in the padding that starts at lg — at md the
        panes cover the shell edge to edge, so the blur is composited there for
        nothing. Hence the glass is an lg treatment and md stays opaque. */}
    <div className="relative flex h-full w-full overflow-hidden bg-white md:rounded-shell md:border md:border-white/60 md:shadow-stage lg:gap-2.5 lg:bg-white/55 lg:p-2.5 lg:backdrop-blur-2xl">
      <MailboxNav visible={pane === 'nav'} accounts={accounts} mailboxes={mailboxes} selectedAccount={selectedAccount} selectedMailbox={selectedMailbox} unreadCount={unreadCount} foldersCollapsed={foldersCollapsed}
        onCompose={() => compose()}
        onSelectAll={selectAll}
        onSelectAccount={selectAccount}
        onSelectMailbox={id => { setSelectedMailbox(id); setPane('list') }}
        onShowOutbox={() => setShowOutbox(true)} onShowAccounts={() => setShowAccounts(true)} onShowSettings={() => setShowSettings(true)} onLogout={logout} />

      <MessageList visible={pane === 'list'} title={listTitle} messages={messages} accountMap={accountMap} selected={selected} unreadCount={unreadCount}
        markingRead={markingRead} loading={loading} error={error} query={query} cursor={cursor}
        onOpenNav={() => setPane('nav')} onMarkViewRead={markViewRead} onRefresh={refresh} onQueryChange={setQuery}
        onOpen={openMessage} onLoadMore={() => loadMessages(true, cursor)} revealID={revealID} />

      {/* The reading pane is the top layer: pure white and the strongest shadow of
          the three. The radius and shadow are inline rather than via .pane-light so
          the higher elevation cannot be overwritten by that class's own lg rule. */}
      <main className={`${pane === 'detail' ? 'flex' : 'hidden'} md:flex min-w-0 flex-1 flex-col overflow-hidden bg-white lg:rounded-panel lg:shadow-glass-high`}>
        {selected ? <MessageDetail selected={selected} details={details} autoLoadRemoteImages={preferences.autoLoadRemoteImages} onBack={() => setPane('list')} onStar={() => mutateMessage({ is_starred: !selected.is_starred })} onArchive={() => mutateMessage({ archive: true })} onReply={() => compose()} onNotice={announce} /> : <Welcome count={unreadCount} />}
      </main>
    </div>
    {showAccounts && <AccountDialog onClose={() => setShowAccounts(false)} onCreated={() => { setShowAccounts(false); loadAccounts() }} />}
    {showOutbox && <OutboxDialog onClose={() => setShowOutbox(false)} onEdit={draft => { setShowOutbox(false); compose(draft) }} />}
    {showComposer && <Composer accounts={accounts} replyTo={composerDraft ? null : selected} initialDraft={composerDraft} onClose={() => setShowComposer(false)} onSent={() => { setShowComposer(false); refresh() }} />}
    {showSettings && <SettingsDialog preferences={preferences} accounts={accounts} onChange={updatePreferences} onClose={() => setShowSettings(false)} onAddAccount={() => { setShowSettings(false); setShowAccounts(true) }} onDeleted={forgetAccount} onLogout={logout} />}
    {toast && <div role="status" className="pointer-events-none fixed bottom-6 left-1/2 z-50 -translate-x-1/2 rounded-full bg-pine px-5 py-2.5 text-xs font-semibold text-white shadow-lift-4">{toast}</div>}
  </div>
}
