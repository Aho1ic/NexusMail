import { CheckCheck, LoaderCircle, Menu, Paperclip, RefreshCw, Search, Star, X } from 'lucide-react'
import { displaySender, formatDate } from '../lib/format'
import type { Account, Message } from '../types'
import { EmptyState } from './shared'

type Props = {
  visible: boolean
  title?: string
  messages: Message[]
  accountMap: Map<number, Account>
  selected: Message | null
  unreadCount: number
  markingRead: boolean
  loading: boolean
  error: string
  query: string
  cursor?: string
  onOpenNav: () => void
  onMarkViewRead: () => void
  onRefresh: () => void
  onQueryChange: (value: string) => void
  onOpen: (message: Message) => void
  onLoadMore: () => void
}

export function MessageList({ visible, title, messages, accountMap, selected, unreadCount, markingRead, loading, error, query, cursor, onOpenNav, onMarkViewRead, onRefresh, onQueryChange, onOpen, onLoadMore }: Props) {
  // The divider only exists while the panes are flush; from lg they are separate
  // cards and the gap does that job. The background stays opaque — a translucent
  // one here would force the shell's blur to repaint on every scrolled row.
  return <section className={`${visible ? 'flex' : 'hidden'} md:flex pane-light w-full md:w-[390px] lg:w-[440px] shrink-0 flex-col overflow-hidden border-r border-black/5 bg-[#fbfaf6] lg:border-r-0`}>
    <header className="border-b border-black/5 px-5 pb-4 pt-5">
      <div className="flex items-center justify-between"><button onClick={onOpenNav} aria-label="打开文件夹" className="md:hidden"><Menu size={21} /></button><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Nexus stream</p><h1 className="font-serif text-2xl">{title}</h1></div><div className="flex gap-1"><button onClick={onMarkViewRead} disabled={markingRead || unreadCount === 0} className="icon-button disabled:opacity-35" title={unreadCount ? `将当前视图的 ${unreadCount} 封未读邮件标记为已读` : '当前视图没有未读邮件'} aria-label="全部已读">{markingRead ? <LoaderCircle size={17} className="animate-spin" /> : <CheckCheck size={17} />}</button><button onClick={onRefresh} className="icon-button" aria-label="刷新"><RefreshCw size={17} className={loading ? 'animate-spin' : ''} /></button></div></div>
      <div className="relative mt-4"><Search className="absolute left-3 top-1/2 -translate-y-1/2 text-black/30" size={16} /><input value={query} onChange={event => onQueryChange(event.target.value)} className="w-full rounded-2xl border border-black/5 bg-white py-2.5 pl-9 pr-8 text-sm shadow-lift-1 outline-none ring-pine/20 transition focus:shadow-lift-2 focus:ring-2" placeholder="搜索主题、发件人或正文…" />{query && <button onClick={() => onQueryChange('')} className="absolute right-2.5 top-1/2 -translate-y-1/2 text-black/30"><X size={15} /></button>}</div>
    </header>
    <div className="flex-1 overflow-y-auto p-2" aria-label="邮件列表">
      {error && <div className="m-3 rounded-card bg-red-50 p-3 text-xs text-red-700 shadow-lift-1">{error}</div>}
      {!loading && messages.length === 0 && <EmptyState />}
      {messages.map(message => <MessageRow key={message.id} message={message} account={accountMap.get(message.account_id)} active={selected?.id === message.id} onClick={() => onOpen(message)} />)}
      {cursor && <button disabled={loading} onClick={onLoadMore} className="my-3 w-full rounded-2xl py-3 text-xs font-semibold text-pine/60 transition hover:bg-sage/40">{loading ? '加载中…' : '加载更多'}</button>}
    </div>
  </section>
}

function MessageRow({ message, account, active, onClick }: { message: Message; account?: Account; active: boolean; onClick: () => void }) {
  // The lift is only offered to inactive rows: the selected row already sits at a
  // fixed higher elevation, and .row-lift's hover shadow would drop it back down.
  return <button onClick={onClick} style={{ contentVisibility: 'auto', containIntrinsicSize: '144px' }} className={`group relative mb-1 w-full rounded-card p-4 text-left ${active ? 'bg-sage shadow-lift-2 transition' : 'row-lift hover:bg-white'} ${!message.is_read ? 'bg-white' : ''}`}>
    {!message.is_read && <span className="absolute left-1.5 top-6 h-1.5 w-1.5 rounded-full bg-coral" />}
    <div className="flex items-start justify-between gap-3"><div className={`truncate text-sm ${!message.is_read ? 'font-bold' : 'font-medium text-black/65'}`}>{displaySender(message.sender)}</div><time className="shrink-0 text-[10px] text-black/35">{formatDate(message.received_at)}</time></div>
    <div className={`mt-1 truncate text-sm ${!message.is_read ? 'font-semibold' : 'text-black/55'}`}>{message.subject || '（无主题）'}</div>
    <p className="mt-1.5 line-clamp-2 text-xs leading-5 text-black/40">{message.snippet || '正文尚未同步'}</p>
    <div className="mt-3 flex items-center justify-between"><span className="rounded-full bg-black/[.04] px-2 py-1 text-[9px] font-bold uppercase tracking-wide text-black/35">{account?.display_name || account?.provider || 'Mail'}</span><div className="flex gap-2 text-black/25">{message.has_attachments && <Paperclip size={13} />}{message.is_starred && <Star size={13} className="fill-amber-400 text-amber-400" />}</div></div>
  </button>
}
