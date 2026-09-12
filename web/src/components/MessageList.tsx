import { useEffect, useRef } from 'react'
import { CheckCheck, LoaderCircle, Menu, Paperclip, RefreshCw, Search, Star, X } from 'lucide-react'
import { displaySender, formatDate } from '../lib/format'
import { accountColor, chipStyle } from '../lib/accountColors'
import type { Account, Message } from '../types'
import { EmptyState } from './shared'
import { providerLabel } from './providers'

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
  accountColors?: Record<string, string>
  onOpenNav: () => void
  onMarkViewRead: () => void
  onRefresh: () => void
  onQueryChange: (value: string) => void
  onOpen: (message: Message) => void
  onLoadMore: () => void
  revealID: number | null
}

export function MessageList({ visible, title, messages, accountMap, selected, unreadCount, markingRead, loading, error, query, cursor, accountColors, onOpenNav, onMarkViewRead, onRefresh, onQueryChange, onOpen, onLoadMore, revealID }: Props) {
  // The divider only exists while the panes are flush; from lg they are separate
  // cards and the gap does that job. The background stays opaque — a translucent
  // one here would force the shell's blur to repaint on every scrolled row.
  return <section className={`${visible ? 'flex' : 'hidden'} md:flex pane-light w-full md:w-[390px] lg:w-[440px] shrink-0 flex-col overflow-hidden border-r border-black/5 bg-[#fbfaf6] lg:border-r-0`}>
    <header className="border-b border-black/5 px-5 pb-4 pt-5">
      <div className="flex items-center justify-between"><button onClick={onOpenNav} aria-label="打开文件夹" className="md:hidden"><Menu size={21} /></button><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Nexus stream</p><h1 className="font-serif text-2xl">{title}</h1></div><div className="flex gap-1"><button onClick={onMarkViewRead} disabled={markingRead || unreadCount === 0} className="icon-button disabled:opacity-35" title={unreadCount ? `将当前视图的 ${unreadCount} 封未读邮件标记为已读` : '当前视图没有未读邮件'} aria-label="全部已读">{markingRead ? <LoaderCircle size={17} className="animate-spin" /> : <CheckCheck size={17} />}</button><button onClick={onRefresh} className="icon-button" aria-label="刷新"><RefreshCw size={17} className={loading ? 'animate-spin' : ''} /></button></div></div>
      <div className="relative mt-4"><Search className="absolute left-3 top-1/2 -translate-y-1/2 text-black/30" size={16} /><input value={query} onChange={event => onQueryChange(event.target.value)} className="w-full rounded-2xl border border-black/5 bg-white py-2.5 pl-9 pr-8 text-sm shadow-lift-1 outline-none ring-pine/20 transition focus:shadow-lift-2 focus:ring-2" placeholder="搜索主题、发件人或正文…" />{query && <button onClick={() => onQueryChange('')} className="absolute right-2.5 top-1/2 -translate-y-1/2 text-black/30"><X size={15} /></button>}</div>
    </header>
    {/* aria-label sat on a bare div with no role, so it named nothing and never
        reached assistive tech. The label belongs to the list, and the rows are its
        items; the error, the empty state and the load-more button are not, so the
        role goes on an inner wrapper rather than the scroll container. aria-busy
        covers the fetch, during which the pane renders neither rows nor the empty
        state and would otherwise be a silently blank region. */}
    <div className="flex-1 overflow-y-auto p-2" aria-busy={loading}>
      {error && <div role="alert" className="m-3 rounded-card bg-red-50 p-3 text-xs text-red-700 shadow-lift-1">{error}</div>}
      {!loading && messages.length === 0 && <EmptyState />}
      <div role="list" aria-label="邮件列表">
        {messages.map(message => <div role="listitem" key={message.id}><MessageRow message={message} account={accountMap.get(message.account_id)} accountColors={accountColors} active={selected?.id === message.id} revealed={revealID === message.id} onClick={() => onOpen(message)} /></div>)}
      </div>
      {cursor && <button disabled={loading} onClick={onLoadMore} className="my-3 w-full rounded-2xl py-3 text-xs font-semibold text-pine/60 transition hover:bg-sage/40">{loading ? '加载中…' : '加载更多'}</button>}
    </div>
  </section>
}

function MessageRow({ message, account, accountColors, active, revealed, onClick }: { message: Message; account?: Account; accountColors?: Record<string, string>; active: boolean; revealed: boolean; onClick: () => void }) {
  const node = useRef<HTMLButtonElement>(null)
  // The revealed row is the one the user was sent here to find, and it is normally
  // below the fold — jumping to it is the whole point of the trip, so it happens on
  // render rather than waiting for a scroll the user has no reason to make.
  useEffect(() => {
    if (!revealed) return
    const row = node.current
    if (!row) return
    // scrollIntoView centres the row against EVERY scrollable ancestor, the
    // viewport included, so the whole stage shifted along with the list. Walk up to
    // the list's own scroll container and move only that one.
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
  // The lift is only offered to inactive rows: the selected row already sits at a
  // fixed higher elevation, and .row-lift's hover shadow would drop it back down.
  return <button ref={node} onClick={onClick} data-revealed={revealed ? '' : undefined} style={{ contentVisibility: 'auto', containIntrinsicSize: '144px' }} className={`group relative mb-1 w-full rounded-card p-4 text-left ${active ? 'bg-sage shadow-lift-2 transition' : 'row-lift hover:bg-white'} ${!message.is_read ? 'bg-white' : ''} ${revealed ? 'ring-2 ring-coral' : ''}`}>
    {/* The dot rides beside the timestamp instead of the row's left edge: at the edge
        it read as list furniture, and the elevation difference alone was too quiet to
        find one unread row among forty. The span is always laid out, invisible when
        read, so timestamps stay in one column. */}
    <div className="flex items-start justify-between gap-3"><div className={`truncate text-sm ${!message.is_read ? 'font-bold' : 'font-medium text-black/65'}`}>{displaySender(message.sender)}</div><div className="flex shrink-0 items-center gap-1.5"><time className="text-[10px] text-black/35">{formatDate(message.received_at)}</time><span className={`h-2 w-2 rounded-full ${message.is_read ? 'invisible' : 'bg-coral'}`} aria-hidden /></div></div>
    {!message.is_read && <span className="sr-only">未读</span>}
    <div className={`mt-1 truncate text-sm ${!message.is_read ? 'font-semibold' : 'text-black/55'}`}>{message.subject || '（无主题）'}</div>
    <p className="mt-1.5 line-clamp-2 text-xs leading-5 text-black/40">{message.snippet || '正文尚未同步'}</p>
    <div className="mt-3 flex items-center justify-between">{account
      ? <span style={chipStyle(accountColor(account.id, accountColors))} className="rounded-full px-2 py-1 text-[9px] font-bold tracking-wide">{account.display_name || providerLabel(account.provider)}</span>
      : <span className="rounded-full bg-black/[.04] px-2 py-1 text-[9px] font-bold tracking-wide text-black/35">Mail</span>}<div className="flex gap-2 text-black/25">{message.has_attachments && <Paperclip size={13} />}{message.is_starred && <Star size={13} className="fill-amber-400 text-amber-400" />}</div></div>
  </button>
}
