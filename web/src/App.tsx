import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Archive, AtSign, Bell, ChevronDown, Circle, File, Inbox, LoaderCircle,
  LogOut, Mail, Menu, Paperclip, Plus, RefreshCw, Search, Send, Settings, Star, X,
  SquarePen,
} from 'lucide-react'
import { APIError, api, isAuthenticated } from './lib/api'
import type { Account, Attachment, Draft, DraftInput, EventEnvelope, Mailbox, Message } from './types'

type Pane = 'nav' | 'list' | 'detail'

export default function App() {
  const [authenticated, setAuthenticated] = useState(isAuthenticated())
  if (!authenticated) return <Login onAuthenticated={() => setAuthenticated(true)} />
  return <MailboxApp onLogout={() => setAuthenticated(false)} />
}

function Login({ onAuthenticated }: { onAuthenticated: () => void }) {
  const [key, setKey] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError('')
    try {
      await api.login(key)
      if ('Notification' in window && Notification.permission === 'default') void Notification.requestPermission()
      onAuthenticated()
    } catch (err) { setError(messageOf(err)) } finally { setBusy(false) }
  }
  return <main className="min-h-screen bg-paper text-ink grid place-items-center overflow-hidden relative">
    <div className="absolute -top-32 -right-24 h-96 w-96 rounded-full bg-sage blur-3xl opacity-80" />
    <div className="absolute -bottom-36 -left-24 h-96 w-96 rounded-full bg-orange-100 blur-3xl opacity-70" />
    <form onSubmit={submit} className="relative w-[min(92vw,420px)] rounded-[2rem] border border-black/5 bg-white/80 p-9 shadow-panel backdrop-blur-xl">
      <Brand />
      <h1 className="mt-10 font-serif text-4xl leading-tight">欢迎回到你的<br />统一收件箱。</h1>
      <p className="mt-3 text-sm leading-6 text-black/50">输入部署时配置的主 API Key，凭据只用于换取安全的浏览器会话。</p>
      <label htmlFor="api-key" className="mt-8 block text-xs font-semibold uppercase tracking-[.18em] text-black/45">API Key</label>
      <input id="api-key" autoFocus type="password" value={key} onChange={event => setKey(event.target.value)} className="input mt-2" placeholder="至少 32 个字符" />
      {error && <p className="mt-3 text-sm text-red-600">{error}</p>}
      <button disabled={busy || key.length < 32} className="button-primary mt-6 w-full">{busy ? <LoaderCircle className="animate-spin" size={18} /> : '进入 NexusMail'}</button>
    </form>
  </main>
}

function MailboxApp({ onLogout }: { onLogout: () => void }) {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [mailboxes, setMailboxes] = useState<Mailbox[]>([])
  const [selectedAccount, setSelectedAccount] = useState<number | null>(null)
  const [selectedMailbox, setSelectedMailbox] = useState<number | null>(null)
  const [messages, setMessages] = useState<Message[]>([])
  const [selected, setSelected] = useState<Message | null>(null)
  const [details, setDetails] = useState<{ message: Message; attachments: Attachment[] } | null>(null)
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [cursor, setCursor] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [pane, setPane] = useState<Pane>('list')
  const [showAccounts, setShowAccounts] = useState(false)
  const [showComposer, setShowComposer] = useState(false)
  const [composerDraft, setComposerDraft] = useState<Draft | null>(null)
  const [showOutbox, setShowOutbox] = useState(false)

  const compose = useCallback((draft: Draft | null = null) => { setComposerDraft(draft); setShowComposer(true) }, [])

  const loadAccounts = useCallback(async () => {
    try { const result = await api.accounts(); setAccounts(result.items) }
    catch (err) { if (err instanceof APIError && err.status === 401) onLogout(); else setError(messageOf(err)) }
  }, [onLogout])

  const loadMailboxes = useCallback(async (accountID: number | null) => {
    if (!accountID) { setMailboxes([]); return }
    try { setMailboxes((await api.mailboxes(accountID)).items) } catch (err) { setError(messageOf(err)) }
  }, [])

  const loadMessages = useCallback(async (append = false, nextCursor?: string) => {
    setLoading(true); setError('')
    const params = new URLSearchParams({ limit: '40' })
    if (selectedAccount) params.set('account_id', String(selectedAccount))
    if (selectedMailbox) params.set('mailbox_id', String(selectedMailbox)); else params.set('folder', 'inbox')
    if (debouncedQuery) params.set('query', debouncedQuery)
    if (nextCursor) params.set('cursor', nextCursor)
    try {
      const page = await api.messages(params)
      setMessages(current => append ? [...current, ...page.items.filter(item => !current.some(existing => existing.id === item.id))] : page.items)
      setCursor(page.next_cursor)
    } catch (err) { setError(messageOf(err)) } finally { setLoading(false) }
  }, [selectedAccount, selectedMailbox, debouncedQuery])

  useEffect(() => { loadAccounts() }, [loadAccounts])
  useEffect(() => { loadMailboxes(selectedAccount); setSelectedMailbox(null) }, [selectedAccount, loadMailboxes])
  useEffect(() => { const timer = window.setTimeout(() => setDebouncedQuery(query.trim()), 250); return () => clearTimeout(timer) }, [query])
  useEffect(() => { loadMessages() }, [loadMessages])

  const refresh = useCallback(() => { loadAccounts(); loadMailboxes(selectedAccount); loadMessages() }, [loadAccounts, loadMailboxes, loadMessages, selectedAccount])
  useRealtime(refresh)

  async function openMessage(message: Message) {
    setSelected(message); setPane('detail'); setDetails(null)
    if (!message.is_read) {
      setMessages(items => items.map(item => item.id === message.id ? { ...item, is_read: true } : item))
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

  useKeyboard(messages, selected, openMessage, () => compose(), () => mutateMessage({ archive: true }))

  async function logout() { try { await api.logout() } finally { onLogout() } }
  const accountMap = useMemo(() => new Map(accounts.map(account => [account.id, account])), [accounts])

  return <div className="h-screen bg-paper text-ink p-0 md:p-3 lg:p-5 overflow-hidden">
    <div className="mx-auto flex h-full max-w-[1680px] overflow-hidden bg-white md:rounded-[1.8rem] md:border md:border-black/5 md:shadow-panel">
      <aside className={`${pane === 'nav' ? 'flex' : 'hidden'} md:flex w-full md:w-[260px] shrink-0 flex-col bg-pine text-white`}>
        <div className="p-6"><Brand light /></div>
        <button onClick={() => compose()} className="mx-5 mt-3 flex items-center justify-center gap-2 rounded-2xl bg-coral px-5 py-3.5 text-sm font-semibold shadow-lg shadow-black/10 transition hover:-translate-y-0.5"><SquarePen size={18} />写邮件</button>
        <nav className="mt-8 flex-1 overflow-y-auto px-3">
          <NavItem active={!selectedAccount && !selectedMailbox} icon={<Inbox size={18} />} label="All Inboxes" count={messages.filter(m => !m.is_read).length} onClick={() => { setSelectedAccount(null); setSelectedMailbox(null); setPane('list') }} />
          <NavItem active={false} icon={<Send size={18} />} label="草稿与发件箱" onClick={() => setShowOutbox(true)} />
          <div className="mt-7 px-3 text-[10px] font-bold uppercase tracking-[.22em] text-white/40">账户</div>
          {accounts.map(account => <div key={account.id}>
            <NavItem active={selectedAccount === account.id && !selectedMailbox} icon={<span className={`h-2.5 w-2.5 rounded-full ${account.status === 'connected' ? 'bg-emerald-300' : account.status === 'backoff' ? 'bg-amber-300' : 'bg-white/30'}`} />} label={account.display_name || account.email} sublabel={account.email} onClick={() => { setSelectedAccount(account.id); setSelectedMailbox(null); setPane('list') }} />
            {selectedAccount === account.id && mailboxes.map(box => <button key={box.id} onClick={() => { setSelectedMailbox(box.id); setPane('list') }} className={`ml-8 flex w-[calc(100%-2.5rem)] items-center gap-2 rounded-xl px-3 py-2 text-left text-xs ${selectedMailbox === box.id ? 'bg-white/12 text-white' : 'text-white/50 hover:text-white'}`}><FolderIcon role={box.role} />{box.display_name}</button>)}
          </div>)}
          <button onClick={() => setShowAccounts(true)} className="mt-4 flex w-full items-center gap-3 rounded-xl px-3 py-3 text-sm text-white/50 hover:bg-white/5 hover:text-white"><Plus size={18} />连接邮箱</button>
        </nav>
        <div className="flex items-center justify-between border-t border-white/10 p-5 text-white/50"><button className="hover:text-white"><Settings size={18} /></button><button onClick={logout} className="flex items-center gap-2 text-xs hover:text-white"><LogOut size={16} />退出</button></div>
      </aside>

      <section className={`${pane === 'list' ? 'flex' : 'hidden'} md:flex w-full md:w-[390px] lg:w-[440px] shrink-0 flex-col border-r border-black/5 bg-[#fbfaf6]`}>
        <header className="border-b border-black/5 px-5 pb-4 pt-5">
          <div className="flex items-center justify-between"><button onClick={() => setPane('nav')} className="md:hidden"><Menu size={21} /></button><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Nexus stream</p><h1 className="font-serif text-2xl">{selectedMailbox ? mailboxes.find(box => box.id === selectedMailbox)?.display_name : selectedAccount ? accountMap.get(selectedAccount)?.display_name || accountMap.get(selectedAccount)?.email : 'All Inboxes'}</h1></div><button onClick={refresh} className="icon-button"><RefreshCw size={17} className={loading ? 'animate-spin' : ''} /></button></div>
          <div className="relative mt-4"><Search className="absolute left-3 top-1/2 -translate-y-1/2 text-black/30" size={16} /><input value={query} onChange={event => setQuery(event.target.value)} className="w-full rounded-xl border border-black/5 bg-white py-2.5 pl-9 pr-8 text-sm outline-none ring-pine/20 focus:ring-2" placeholder="搜索主题、发件人或正文…" />{query && <button onClick={() => setQuery('')} className="absolute right-2.5 top-1/2 -translate-y-1/2 text-black/30"><X size={15} /></button>}</div>
        </header>
        <div className="flex-1 overflow-y-auto p-2" aria-label="邮件列表">
          {error && <div className="m-3 rounded-xl bg-red-50 p-3 text-xs text-red-700">{error}</div>}
          {!loading && messages.length === 0 && <EmptyState />}
          {messages.map(message => <MessageRow key={message.id} message={message} account={accountMap.get(message.account_id)} active={selected?.id === message.id} onClick={() => openMessage(message)} />)}
          {cursor && <button disabled={loading} onClick={() => loadMessages(true, cursor)} className="my-3 w-full rounded-xl py-3 text-xs font-semibold text-pine/60 hover:bg-sage/40">{loading ? '加载中…' : '加载更多'}</button>}
        </div>
      </section>

      <main className={`${pane === 'detail' ? 'flex' : 'hidden'} md:flex min-w-0 flex-1 flex-col bg-white`}>
        {selected ? <MessageDetail selected={selected} details={details} onBack={() => setPane('list')} onStar={() => mutateMessage({ is_starred: !selected.is_starred })} onArchive={() => mutateMessage({ archive: true })} onReply={() => compose()} /> : <Welcome count={messages.filter(item => !item.is_read).length} />}
      </main>
    </div>
    {showAccounts && <AccountDialog onClose={() => setShowAccounts(false)} onCreated={() => { setShowAccounts(false); loadAccounts() }} />}
    {showOutbox && <OutboxDialog onClose={() => setShowOutbox(false)} onEdit={draft => { setShowOutbox(false); compose(draft) }} />}
    {showComposer && <Composer accounts={accounts} replyTo={composerDraft ? null : selected} initialDraft={composerDraft} onClose={() => setShowComposer(false)} onSent={() => { setShowComposer(false); refresh() }} />}
  </div>
}

function MessageRow({ message, account, active, onClick }: { message: Message; account?: Account; active: boolean; onClick: () => void }) {
  return <button onClick={onClick} style={{ contentVisibility: 'auto', containIntrinsicSize: '144px' }} className={`group relative mb-1 w-full rounded-2xl p-4 text-left transition ${active ? 'bg-sage shadow-sm' : 'hover:bg-white'} ${!message.is_read ? 'bg-white' : ''}`}>
    {!message.is_read && <span className="absolute left-1.5 top-6 h-1.5 w-1.5 rounded-full bg-coral" />}
    <div className="flex items-start justify-between gap-3"><div className={`truncate text-sm ${!message.is_read ? 'font-bold' : 'font-medium text-black/65'}`}>{displaySender(message.sender)}</div><time className="shrink-0 text-[10px] text-black/35">{formatDate(message.received_at)}</time></div>
    <div className={`mt-1 truncate text-sm ${!message.is_read ? 'font-semibold' : 'text-black/55'}`}>{message.subject || '（无主题）'}</div>
    <p className="mt-1.5 line-clamp-2 text-xs leading-5 text-black/40">{message.snippet || '正文尚未同步'}</p>
    <div className="mt-3 flex items-center justify-between"><span className="rounded-full bg-black/[.04] px-2 py-1 text-[9px] font-bold uppercase tracking-wide text-black/35">{account?.display_name || account?.provider || 'Mail'}</span><div className="flex gap-2 text-black/25">{message.has_attachments && <Paperclip size={13} />}{message.is_starred && <Star size={13} className="fill-amber-400 text-amber-400" />}</div></div>
  </button>
}

function MessageDetail({ selected, details, onBack, onStar, onArchive, onReply }: { selected: Message; details: { message: Message; attachments: Attachment[] } | null; onBack: () => void; onStar: () => void; onArchive: () => void; onReply: () => void }) {
  const message = details?.message ?? selected
  const bodyHTML = message.body_html ?? ''
  const [loadRemoteImages, setLoadRemoteImages] = useState(false)
  useEffect(() => setLoadRemoteImages(false), [message.id])
  const renderedHTML = useMemo(() => prepareMessageHTML(bodyHTML, details?.attachments ?? [], message.id, loadRemoteImages), [bodyHTML, details?.attachments, message.id, loadRemoteImages])
  const hasRemoteImages = bodyHTML.includes('data-nexusmail-remote-src')
  return <>
    <header className="flex items-center justify-between border-b border-black/5 px-5 py-4"><button onClick={onBack} className="md:hidden icon-button"><ChevronDown className="rotate-90" size={19} /></button><div className="flex gap-1"><button onClick={onArchive} className="icon-button" title="归档 (e)"><Archive size={18} /></button><button onClick={onStar} className="icon-button" title="星标"><Star size={18} className={message.is_starred ? 'fill-amber-400 text-amber-400' : ''} /></button></div><button onClick={onReply} className="button-secondary"><SquarePen size={16} />回复</button></header>
    <article className="flex-1 overflow-y-auto px-6 py-8 lg:px-12 xl:px-16">
      <div className="mx-auto max-w-3xl"><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">{formatFullDate(message.received_at)}</p><h1 className="mt-3 font-serif text-3xl leading-tight lg:text-4xl">{message.subject || '（无主题）'}</h1>
        <div className="mt-7 flex items-center gap-3 border-b border-black/5 pb-6"><Avatar label={displaySender(message.sender)} /><div className="min-w-0"><div className="truncate text-sm font-bold">{displaySender(message.sender)}</div><div className="truncate text-xs text-black/40">发给 {message.recipients || '我'}</div></div></div>
        {!details && <div className="grid h-52 place-items-center"><LoaderCircle className="animate-spin text-pine/40" /></div>}
        {details && message.body_state !== 'ready' && <div className="my-10 rounded-2xl bg-sage/50 p-5 text-sm text-pine">正文正在从邮件服务商异步获取，稍后会自动刷新。</div>}
        {details && hasRemoteImages && !loadRemoteImages && <button onClick={() => setLoadRemoteImages(true)} className="mt-6 rounded-xl bg-amber-50 px-4 py-2 text-xs font-semibold text-amber-800">本邮件包含已阻止的远程图片，点击临时加载</button>}
        {details && message.body_html ? <iframe title="邮件正文" sandbox="" referrerPolicy="no-referrer" srcDoc={`<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer"><style>body{font:15px/1.75 system-ui;color:#24332d;overflow-wrap:anywhere}img{max-width:100%;height:auto}a{color:#256b50}pre{white-space:pre-wrap}</style>${renderedHTML}`} className="mt-8 min-h-[520px] w-full border-0" /> : details && <pre className="mt-8 whitespace-pre-wrap font-sans text-[15px] leading-7 text-black/75">{message.body_text || message.snippet}</pre>}
        {!!details?.attachments.length && <div className="mt-10 border-t border-black/5 pt-5"><h2 className="text-xs font-bold uppercase tracking-wider text-black/40">附件</h2><div className="mt-3 grid gap-2 sm:grid-cols-2">{details.attachments.map(att => <a key={att.id} href={`/api/v1/messages/${message.id}/attachments/${att.id}`} className="flex items-center gap-3 rounded-xl border border-black/5 p-3 hover:bg-paper"><span className="grid h-9 w-9 place-items-center rounded-lg bg-sage"><File size={17} /></span><span className="min-w-0"><span className="block truncate text-xs font-semibold">{att.filename}</span><span className="text-[10px] text-black/35">{formatBytes(att.size_bytes)}</span></span></a>)}</div></div>}
      </div>
    </article>
  </>
}

function Composer({ accounts, replyTo, initialDraft, onClose, onSent }: { accounts: Account[]; replyTo: Message | null; initialDraft: Draft | null; onClose: () => void; onSent: () => void }) {
  const [draft, setDraft] = useState<Draft | null>(initialDraft)
  const [accountID, setAccountID] = useState(initialDraft?.account_id ?? accounts[0]?.id ?? 0)
  const [to, setTo] = useState(initialDraft ? decodeAddressList(initialDraft.to).join(', ') : replyTo?.sender.match(/[\w.+-]+@[\w.-]+/)?.[0] ?? '')
  const [cc, setCC] = useState(initialDraft ? decodeAddressList(initialDraft.cc).join(', ') : '')
  const [bcc, setBCC] = useState(initialDraft ? decodeAddressList(initialDraft.bcc).join(', ') : '')
  const [subject, setSubject] = useState(initialDraft?.subject ?? (replyTo ? `Re: ${replyTo.subject.replace(/^Re:\s*/i, '')}` : ''))
  const [body, setBody] = useState(initialDraft?.body_text ?? (replyTo ? `\n\n--- 原邮件 ---\n${replyTo.snippet}` : ''))
  const [busy, setBusy] = useState(false)
  const [status, setStatus] = useState('尚未保存')
  const timer = useRef<number | undefined>(undefined)
  const input: DraftInput = useMemo(() => ({ account_id: accountID, to: splitEmails(to), cc: splitEmails(cc), bcc: splitEmails(bcc), subject, body_text: body }), [accountID, to, cc, bcc, subject, body])

  useEffect(() => {
    if (!accountID) return
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(async () => {
      try {
        setStatus('保存中…')
        const saved = draft ? await api.updateDraft(draft.id, draft.revision, input) : await api.createDraft(input)
        setDraft(saved); setStatus(saved.remote_sync_state === 'synced' ? '已同步到远端' : '已保存，等待远端同步')
      } catch (err) { setStatus(messageOf(err)) }
    }, 2000)
    return () => clearTimeout(timer.current)
  }, [input]) // eslint-disable-line react-hooks/exhaustive-deps

  async function send() {
    setBusy(true)
    try {
      const saved = draft ? await api.updateDraft(draft.id, draft.revision, input) : await api.createDraft(input)
      await api.sendDraft(saved.id); onSent()
    } catch (err) { setStatus(messageOf(err)); setBusy(false) }
  }
  async function attach(file?: File) {
    if (!file) return
    try { const saved = draft ?? await api.createDraft(input); setDraft(saved); await api.uploadAttachment(saved.id, file); setStatus(`已添加 ${file.name}`) } catch (err) { setStatus(messageOf(err)) }
  }
  return <div className="modal-backdrop"><div className="flex h-[min(92vh,760px)] w-[min(94vw,760px)] flex-col overflow-hidden rounded-[1.7rem] bg-white shadow-2xl">
    <header className="flex items-center justify-between border-b border-black/5 px-5 py-4"><div><h2 className="font-serif text-2xl">新邮件</h2><p className="text-[10px] text-black/35">{status}</p></div><button onClick={onClose} className="icon-button"><X size={19} /></button></header>
    <div className="flex-1 overflow-y-auto p-5">
      <select value={accountID} disabled={Boolean(draft)} onChange={event => setAccountID(Number(event.target.value))} className="input mb-2 disabled:opacity-60">{accounts.map(account => <option key={account.id} value={account.id}>{account.display_name || account.email}</option>)}</select>
      <ComposerField label="收件人" value={to} onChange={setTo} placeholder="name@example.com，多个地址用逗号分隔" />
      <div className="grid grid-cols-2 gap-2"><ComposerField label="抄送" value={cc} onChange={setCC} /><ComposerField label="密送" value={bcc} onChange={setBCC} /></div>
      <ComposerField label="主题" value={subject} onChange={setSubject} />
      <textarea value={body} onChange={event => setBody(event.target.value)} className="mt-3 min-h-[320px] w-full resize-none rounded-2xl border border-black/5 bg-paper/50 p-4 text-sm leading-6 outline-none focus:ring-2 focus:ring-pine/20" placeholder="写点什么…" />
    </div>
    <footer className="flex items-center justify-between border-t border-black/5 px-5 py-4"><label className="icon-button cursor-pointer"><Paperclip size={18} /><input type="file" className="hidden" onChange={event => attach(event.target.files?.[0])} /></label><button disabled={busy || !accountID || !to.trim()} onClick={send} className="button-primary">{busy ? <LoaderCircle className="animate-spin" size={17} /> : <Send size={17} />}发送</button></footer>
  </div></div>
}

function OutboxDialog({ onClose, onEdit }: { onClose: () => void; onEdit: (draft: Draft) => void }) {
  const [items, setItems] = useState<Draft[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    setLoading(true)
    try { setItems((await api.drafts()).items); setError('') } catch (err) { setError(messageOf(err)) } finally { setLoading(false) }
  }, [])
  useEffect(() => { load() }, [load])
  async function retry(id: number) { try { await api.retryDraft(id); await load() } catch (err) { setError(messageOf(err)) } }
  async function remove(id: number) { try { await api.deleteDraft(id); await load() } catch (err) { setError(messageOf(err)) } }
  return <div className="modal-backdrop"><div className="flex h-[min(86vh,680px)] w-[min(94vw,680px)] flex-col overflow-hidden rounded-[1.7rem] bg-white shadow-2xl">
    <header className="flex items-center justify-between border-b border-black/5 px-6 py-5"><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Delivery</p><h2 className="font-serif text-3xl">草稿与发件箱</h2></div><div className="flex gap-1"><button onClick={load} className="icon-button"><RefreshCw className={loading ? 'animate-spin' : ''} size={18} /></button><button onClick={onClose} className="icon-button"><X size={19} /></button></div></header>
    <div className="flex-1 overflow-y-auto p-4">
      {error && <p className="m-2 rounded-xl bg-red-50 p-3 text-xs text-red-700">{error}</p>}
      {!loading && items.length === 0 && <div className="grid h-72 place-items-center text-center"><div><Bell className="mx-auto text-pine/20" size={38} /><p className="mt-3 font-serif text-xl">没有待处理邮件</p></div></div>}
      {items.map(draft => <div key={draft.id} className="mb-2 rounded-2xl border border-black/5 bg-paper/50 p-4">
        <div className="flex items-start justify-between gap-4"><div className="min-w-0"><div className="truncate text-sm font-bold">{draft.subject || '（无主题）'}</div><div className="mt-1 truncate text-xs text-black/40">发给 {decodeAddressList(draft.to).join(', ') || '尚未填写'}</div></div><StatusPill status={draft.status} remote={draft.remote_sync_state} /></div>
        {draft.last_error && <p className="mt-3 rounded-lg bg-red-50 p-2 text-[11px] text-red-700">{draft.last_error}</p>}
        <div className="mt-3 flex items-center justify-between text-[10px] text-black/35"><time>{formatFullDate(draft.updated_at)}</time><div className="flex gap-2">{['failed', 'unknown'].includes(draft.status) && <button onClick={() => retry(draft.id)} className="font-semibold text-coral">确认重试</button>}{['draft', 'failed', 'unknown'].includes(draft.status) && <button onClick={() => onEdit(draft)} className="font-semibold text-pine">编辑</button>}{!['queued', 'sending'].includes(draft.status) && <button onClick={() => remove(draft.id)} className="font-semibold text-black/40">删除</button>}</div></div>
      </div>)}
    </div>
  </div></div>
}

function StatusPill({ status, remote }: { status: string; remote: string }) {
  const label: Record<string, string> = { draft: '草稿', queued: '等待发送', sending: '发送中', retry_wait: '等待重试', failed: '失败', unknown: '结果未知', sent: '已发送' }
  const tone = ['failed', 'unknown'].includes(status) ? 'bg-red-50 text-red-700' : status === 'sent' ? 'bg-emerald-50 text-emerald-700' : 'bg-sage text-pine'
  return <span className={`shrink-0 rounded-full px-2.5 py-1 text-[9px] font-bold ${tone}`}>{label[status] || status}{remote === 'conflict' ? ' · 冲突' : ''}</span>
}

function AccountDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [provider, setProvider] = useState('qq'); const [email, setEmail] = useState(''); const [name, setName] = useState(''); const [password, setPassword] = useState(''); const [error, setError] = useState(''); const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent) { event.preventDefault(); setBusy(true); setError(''); try { const result = await api.addAccount({ provider, email, display_name: name, username: email, auth: { type: ['gmail', 'outlook'].includes(provider) ? 'oauth2' : 'password', password } }); if ('authorization_url' in result) { location.href = result.authorization_url; return }; onCreated() } catch (err) { setError(messageOf(err)); setBusy(false) } }
  return <div className="modal-backdrop"><form onSubmit={submit} className="w-[min(92vw,460px)] rounded-[1.7rem] bg-white p-7 shadow-2xl"><div className="flex items-center justify-between"><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Connect</p><h2 className="font-serif text-3xl">连接邮箱</h2></div><button type="button" onClick={onClose} className="icon-button"><X size={19} /></button></div><div className="mt-7 grid grid-cols-4 gap-2">{['qq', '163', 'gmail', 'outlook'].map(item => <button type="button" key={item} onClick={() => setProvider(item)} className={`rounded-xl border px-2 py-3 text-xs font-bold uppercase ${provider === item ? 'border-pine bg-sage text-pine' : 'border-black/5 text-black/35'}`}>{item}</button>)}</div><label className="field-label">显示名称</label><input className="input" value={name} onChange={e => setName(e.target.value)} placeholder="工作邮箱" />{!['gmail', 'outlook'].includes(provider) && <><label className="field-label">邮箱地址</label><input className="input" type="email" required value={email} onChange={e => setEmail(e.target.value)} /><label className="field-label">授权码</label><input className="input" type="password" required value={password} onChange={e => setPassword(e.target.value)} /><p className="mt-2 text-xs text-black/40">请使用邮箱服务商生成的客户端授权码，而非网页登录密码。</p></>}{['gmail', 'outlook'].includes(provider) && <div className="mt-6 rounded-xl bg-sage/50 p-4 text-sm leading-6 text-pine">继续后将跳转到服务商授权页面。部署者必须已配置对应 OAuth Client ID 与 Secret。</div>}{error && <p className="mt-3 text-sm text-red-600">{error}</p>}<button disabled={busy} className="button-primary mt-7 w-full">{busy ? <LoaderCircle className="animate-spin" size={17} /> : '继续'}</button></form></div>
}

function NavItem({ active, icon, label, sublabel, count, onClick }: { active: boolean; icon: React.ReactNode; label: string; sublabel?: string; count?: number; onClick: () => void }) { return <button onClick={onClick} className={`mt-1 flex w-full items-center gap-3 rounded-xl px-3 py-3 text-left transition ${active ? 'bg-white/12 text-white' : 'text-white/65 hover:bg-white/5 hover:text-white'}`}><span className="grid w-5 place-items-center">{icon}</span><span className="min-w-0 flex-1"><span className="block truncate text-sm font-semibold">{label}</span>{sublabel && <span className="block truncate text-[9px] text-white/35">{sublabel}</span>}</span>{Boolean(count) && <span className="rounded-full bg-coral px-2 py-0.5 text-[9px] font-bold text-white">{count}</span>}</button> }
function FolderIcon({ role }: { role: string }) { if (role === 'inbox') return <Inbox size={13} />; if (role === 'sent') return <Send size={13} />; if (role === 'archive') return <Archive size={13} />; return <Mail size={13} /> }
function Brand({ light = false }: { light?: boolean }) { return <div className="flex items-center gap-3"><span className={`grid h-9 w-9 place-items-center rounded-xl ${light ? 'bg-white text-pine' : 'bg-pine text-white'}`}><AtSign size={19} strokeWidth={2.4} /></span><span className="font-serif text-xl font-semibold tracking-tight">NexusMail</span></div> }
function Welcome({ count }: { count: number }) { return <div className="grid h-full place-items-center p-8 text-center"><div><div className="mx-auto grid h-24 w-24 place-items-center rounded-full bg-sage text-pine"><Mail size={38} strokeWidth={1.4} /></div><h2 className="mt-7 font-serif text-3xl">收件箱已就绪</h2><p className="mt-2 text-sm text-black/40">{count ? `还有 ${count} 封未读邮件等待你。` : '一切都处理好了，享受片刻清静。'}</p><div className="mx-auto mt-7 flex w-fit gap-2 text-[10px] text-black/30"><kbd>J</kbd><kbd>K</kbd> 导航 · <kbd>C</kbd> 写信 · <kbd>E</kbd> 归档</div></div></div> }
function EmptyState() { return <div className="grid h-72 place-items-center text-center"><div><Circle className="mx-auto text-pine/20" size={38} /><p className="mt-3 font-serif text-xl">这里空空如也</p><p className="mt-1 text-xs text-black/35">换个文件夹或搜索词试试</p></div></div> }
function Avatar({ label }: { label: string }) { return <span className="grid h-11 w-11 shrink-0 place-items-center rounded-2xl bg-pine font-serif text-lg text-white">{label.trim().charAt(0).toUpperCase() || '?'}</span> }
function ComposerField({ label, value, onChange, placeholder = '' }: { label: string; value: string; onChange: (value: string) => void; placeholder?: string }) { return <label className="mt-2 flex items-center border-b border-black/5 py-2"><span className="w-16 text-xs font-semibold text-black/40">{label}</span><input value={value} onChange={event => onChange(event.target.value)} placeholder={placeholder} className="min-w-0 flex-1 bg-transparent py-1 text-sm outline-none" /></label> }

function prepareMessageHTML(input: string, attachments: Attachment[], messageID: number, loadRemote: boolean) {
  const document = new DOMParser().parseFromString(input, 'text/html')
  const contentIDs = new Map(attachments.filter(item => item.content_id).map(item => [item.content_id!.replace(/[<>]/g, ''), item.id]))
  document.querySelectorAll<HTMLImageElement>('img[src^="cid:"]').forEach(element => {
    const contentID = element.getAttribute('src')?.slice(4).replace(/[<>]/g, '')
    const attachmentID = contentID ? contentIDs.get(contentID) : undefined
    if (attachmentID) element.src = `/api/v1/messages/${messageID}/attachments/${attachmentID}`
  })
  if (loadRemote) document.querySelectorAll<HTMLElement>('[data-nexusmail-remote-src]').forEach(element => {
    const source = element.dataset.nexusmailRemoteSrc
    if (source) element.setAttribute('src', source)
  })
  return document.body.innerHTML
}

function useRealtime(onChange: () => void) {
  useEffect(() => {
    let socket: WebSocket | undefined; let timer = 0; let stopped = false; let delay = 1000
    const connect = () => {
      const scheme = location.protocol === 'https:' ? 'wss' : 'ws'; socket = new WebSocket(`${scheme}://${location.host}/api/v1/ws`)
      socket.onopen = () => { delay = 1000 }
      socket.onmessage = event => { const payload = JSON.parse(event.data) as EventEnvelope; if (['NEW_EMAIL', 'MESSAGE_UPDATED', 'ACCOUNT_STATUS', 'OUTBOX_UPDATED'].includes(payload.type)) { onChange(); if (payload.type === 'NEW_EMAIL' && Notification.permission === 'granted') new Notification('NexusMail 收到新邮件') } }
      socket.onclose = () => { if (!stopped) { timer = window.setTimeout(connect, delay); delay = Math.min(delay * 2, 30000) } }
    }
    connect(); return () => { stopped = true; clearTimeout(timer); socket?.close() }
  }, [onChange])
}

function useKeyboard(messages: Message[], selected: Message | null, open: (message: Message) => void, compose: () => void, archive: () => void) {
  useEffect(() => { const handler = (event: KeyboardEvent) => { if (['INPUT', 'TEXTAREA', 'SELECT'].includes((event.target as HTMLElement).tagName)) return; const index = selected ? messages.findIndex(item => item.id === selected.id) : -1; if (event.key === 'j' && messages[index + 1]) open(messages[index + 1]); if (event.key === 'k' && messages[Math.max(0, index - 1)]) open(messages[Math.max(0, index - 1)]); if (event.key === 'c') compose(); if (event.key === 'e' && selected) archive() }; addEventListener('keydown', handler); return () => removeEventListener('keydown', handler) }, [messages, selected, open, compose, archive])
}

function splitEmails(value: string) { return value.split(/[,;\n]/).map(item => item.trim()).filter(Boolean) }
function decodeAddressList(value: string) { try { const parsed = JSON.parse(value); return Array.isArray(parsed) ? parsed.map(String) : [] } catch { return [] } }
function displaySender(value: string) { return value.replace(/<.*?>/g, '').replace(/["']/g, '').trim() || value }
function formatDate(value: number) { const date = new Date(value); const today = new Date(); return date.toDateString() === today.toDateString() ? date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : date.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' }) }
function formatFullDate(value: number) { return new Date(value).toLocaleString('zh-CN', { dateStyle: 'long', timeStyle: 'short' }) }
function formatBytes(value: number) { if (value < 1024) return `${value} B`; if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`; return `${(value / 1024 / 1024).toFixed(1)} MB` }
function messageOf(error: unknown) { return error instanceof Error ? error.message : '发生未知错误' }
