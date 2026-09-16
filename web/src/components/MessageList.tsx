import { useEffect, useRef } from 'react'
import { CheckCheck, Layers, LoaderCircle, Menu, Paperclip, RefreshCw, Search, Star, X } from 'lucide-react'
import { displaySender, formatDate } from '../lib/format'
import { accountColor, chipStyle } from '../lib/accountColors'
import type { ListEntry } from '../lib/stack'
import type { Account, Message } from '../types'
import { EmptyState } from './shared'
import { providerLabel } from './providers'

type Props = {
  visible: boolean
  title?: string
  entries: ListEntry[]
  accountMap: Map<number, Account>
  selected: Message | null
  selectedStackKey: string | null
  unreadCount: number
  markingRead: boolean
  loading: boolean
  error: string
  query: string
  cursor?: string
  accountColors?: Record<string, string>
  onOpenNav: () => void
  onMarkViewRead: () => void
  onRefresh: () => void
  onQueryChange: (value: string) => void
  onOpen: (message: Message) => void
  onOpenStack: (entry: Extract<ListEntry, { kind: 'stack' }>) => void
  onLoadMore: () => void
  revealID: number | null
}

export function MessageList({ visible, title, entries, accountMap, selected, selectedStackKey, unreadCount, markingRead, loading, error, query, cursor, accountColors, onOpenNav, onMarkViewRead, onRefresh, onQueryChange, onOpen, onOpenStack, onLoadMore, revealID }: Props) {
  return <section className={`${visible ? 'flex' : 'hidden'} md:flex pane-light w-full md:w-[390px] shrink-0 flex-col overflow-hidden border-r border-black/5 bg-[#fbfaf6] lg:border-r-0 xl:w-[420px] 2xl:w-[440px]`}>
    <header className="border-b border-black/5 px-5 pb-4 pt-5">
      <div className="flex items-center justify-between gap-3"><button onClick={onOpenNav} aria-label="打开文件夹" className="shrink-0 lg:hidden"><Menu size={21} /></button><div className="min-w-0 flex-1"><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Nexus stream</p><h1 className="break-words font-serif text-2xl text-balance">{title}</h1></div><div className="flex shrink-0 gap-1"><button onClick={onMarkViewRead} disabled={markingRead || unreadCount === 0} className="icon-button disabled:opacity-35" title={unreadCount ? `将当前视图的 ${unreadCount} 封未读邮件标记为已读` : '当前视图没有未读邮件'} aria-label="全部已读">{markingRead ? <LoaderCircle size={17} className="animate-spin" /> : <CheckCheck size={17} />}</button><button onClick={onRefresh} className="icon-button" aria-label="刷新"><RefreshCw size={17} className={loading ? 'animate-spin' : ''} /></button></div></div>
      <div className="relative mt-4"><Search className="absolute left-3 top-1/2 -translate-y-1/2 text-black/30" size={16} /><input value={query} onChange={event => onQueryChange(event.target.value)} className="w-full rounded-2xl border border-black/5 bg-white py-2.5 pl-9 pr-8 text-sm shadow-lift-1 outline-none ring-pine/20 transition focus:shadow-lift-2 focus:ring-2" placeholder="搜索主题、发件人或正文…" />{query && <button onClick={() => onQueryChange('')} className="absolute right-2.5 top-1/2 -translate-y-1/2 text-black/30"><X size={15} /></button>}</div>
    </header>
    <div className="flex-1 overflow-y-auto p-2" aria-busy={loading}>
      {error && <div role="alert" className="m-3 rounded-card bg-red-50 p-3 text-xs text-red-700 shadow-lift-1">{error}</div>}
      {!loading && entries.length === 0 && <EmptyState />}
      <div role="list" aria-label="邮件列表">
        {entries.map(entry => entry.kind === 'message'
          ? <div role="listitem" key={`m-${entry.message.id}`}><MessageRow message={entry.message} account={accountMap.get(entry.message.account_id)} accountColors={accountColors} active={selected?.id === entry.message.id} revealed={revealID === entry.message.id} onClick={() => onOpen(entry.message)} /></div>
          : <div role="listitem" key={`s-${entry.key}`}><StackRow entry={entry} account={accountMap.get(entry.latest.account_id)} accountColors={accountColors} active={selectedStackKey === entry.key} onClick={() => onOpenStack(entry)} /></div>)}
      </div>
      {cursor && <button disabled={loading} onClick={onLoadMore} className="my-3 w-full rounded-2xl py-3 text-xs font-semibold text-pine/60 transition hover:bg-sage/40">{loading ? '加载中…' : '加载更多'}</button>}
    </div>
  </section>
}

function StackRow({ entry, account, accountColors, active, onClick }: { entry: Extract<ListEntry, { kind: 'stack' }>; account?: Account; accountColors?: Record<string, string>; active: boolean; onClick: () => void }) {
  const latest = entry.latest
  return <div className={`relative mb-2 ${active ? 'is-active' : ''}`}>
    {/* The layered cards sell "stack". When the row is selected the sheet under
        the button must not fight the sage fill — recolour it so the active state
        reads as one solid selected row, not a white card in front of green. */}
    <div aria-hidden className={`absolute inset-x-1 top-1 bottom-0 rounded-card border transition-colors ${active ? 'border-pine/15 bg-sage/70' : entry.unread ? 'border-black/5 bg-white' : 'border-black/5 bg-paper'}`} />
    <button
      onClick={onClick}
      aria-label={`${entry.label} 的 ${entry.messages.length} 封邮件`}
      aria-current={active ? 'true' : undefined}
      className={`relative z-10 w-full rounded-card p-4 text-left transition-colors ${active ? 'bg-sage shadow-lift-2' : entry.unread ? 'bg-white shadow-lift-1' : 'row-lift bg-[#fbfaf6] hover:bg-white'}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2">
          <Layers size={15} className="shrink-0 text-pine/50" />
          <div className={`truncate text-sm ${active || !entry.unread ? 'font-medium text-black/70' : 'font-bold'}`}>{entry.label}</div>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <time className="text-[10px] text-black/35">{formatDate(latest.received_at)}</time>
          <span className={`h-2 w-2 rounded-full ${entry.unread && !active ? 'bg-coral' : 'invisible'}`} aria-hidden />
        </div>
      </div>
      {entry.unread > 0 && <span className="sr-only">{entry.unread} 封未读</span>}
      <div className={`mt-1 truncate text-sm ${active ? 'font-medium text-black/75' : 'font-semibold text-black/70'}`}>{entry.messages.length} 封邮件 · {latest.subject || '（无主题）'}</div>
      <p className="mt-1.5 line-clamp-2 text-xs leading-5 text-black/40">{latest.snippet || '正文尚未同步'}</p>
      <div className="mt-3 flex items-center justify-between">
        {account
          ? <span style={chipStyle(accountColor(account.id, accountColors))} className="rounded-full px-2 py-1 text-[9px] font-bold tracking-wide">{account.display_name || providerLabel(account.provider)}</span>
          : <span className="rounded-full bg-black/[.04] px-2 py-1 text-[9px] font-bold tracking-wide text-black/35">Mail</span>}
        <span className={`rounded-full px-2 py-1 text-[9px] font-bold tracking-wide ${active ? 'bg-white/70 text-pine' : 'bg-pine/10 text-pine'}`}>{entry.messages.length}</span>
      </div>
    </button>
  </div>
}

function MessageRow({ message, account, accountColors, active, revealed, onClick }: { message: Message; account?: Account; accountColors?: Record<string, string>; active: boolean; revealed: boolean; onClick: () => void }) {
  const node = useRef<HTMLButtonElement>(null)
  useEffect(() => {
    if (!revealed) return
    const row = node.current
    if (!row) return
    let box: HTMLElement | null = row.parentElement
    while (box) {
      const overflow = getComputedStyle(box).overflowY
      if (overflow === 'auto' || overflow === 'scroll') break
      box = box.parentElement
    }
    if (!box) { row.scrollIntoView?.({ block: 'center', inline: 'nearest' }); return }
    const boxRect = box.getBoundingClientRect()
    const rowRect = row.getBoundingClientRect()
    const centered = box.scrollTop + rowRect.top - boxRect.top - (boxRect.height - rowRect.height) / 2
    box.scrollTop = Math.max(centered, 0)
  }, [revealed])
  return <button ref={node} onClick={onClick} data-revealed={revealed ? '' : undefined} style={{ contentVisibility: 'auto', containIntrinsicSize: '144px' }} className={`group relative mb-1 w-full rounded-card p-4 text-left ${active ? 'bg-sage shadow-lift-2 transition' : !message.is_read ? 'bg-white' : 'row-lift hover:bg-white'} ${revealed ? 'ring-2 ring-coral' : ''}`}>
    <div className="flex items-start justify-between gap-3"><div className={`truncate text-sm ${!message.is_read ? 'font-bold' : 'font-medium text-black/65'}`}>{displaySender(message.sender)}</div><div className="flex shrink-0 items-center gap-1.5"><time className="text-[10px] text-black/35">{formatDate(message.received_at)}</time><span className={`h-2 w-2 rounded-full ${message.is_read ? 'invisible' : 'bg-coral'}`} aria-hidden /></div></div>
    {!message.is_read && <span className="sr-only">未读</span>}
    <div className={`mt-1 truncate text-sm ${!message.is_read ? 'font-semibold' : 'text-black/55'}`}>{message.subject || '（无主题）'}</div>
    <p className="mt-1.5 line-clamp-2 text-xs leading-5 text-black/40">{message.snippet || '正文尚未同步'}</p>
    <div className="mt-3 flex items-center justify-between">{account
      ? <span style={chipStyle(accountColor(account.id, accountColors))} className="rounded-full px-2 py-1 text-[9px] font-bold tracking-wide">{account.display_name || providerLabel(account.provider)}</span>
      : <span className="rounded-full bg-black/[.04] px-2 py-1 text-[9px] font-bold tracking-wide text-black/35">Mail</span>}<div className="flex gap-2 text-black/25">{message.has_attachments && <Paperclip size={13} />}{message.is_starred && <Star size={13} className="fill-amber-400 text-amber-400" />}</div></div>
  </button>
}
