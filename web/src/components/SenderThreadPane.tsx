import { ArrowLeft, Paperclip, Star } from 'lucide-react'
import { formatDate, formatFullDate } from '../lib/format'
import { senderLabel } from '../lib/sender'
import type { Message } from '../types'

type Props = {
  label: string
  email: string
  messages: Message[]
  loading: boolean
  error?: string
  onBack: () => void
  onOpen: (message: Message) => void
}

// The reading pane while a sender stack is open: one list of that sender's mail,
// then a click opens the real detail the pane normally shows.
export function SenderThreadPane({ label, email, messages, loading, error, onBack, onOpen }: Props) {
  return <>
    <header className="flex items-center justify-between border-b border-black/5 px-5 py-4">
      <button onClick={onBack} className="icon-button" aria-label="返回列表"><ArrowLeft size={18} /></button>
      <div className="min-w-0 flex-1 px-3 text-center">
        <p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Sender stack</p>
        <h1 className="truncate font-serif text-xl">{label}</h1>
        <p className="truncate text-[11px] text-black/40">{email}</p>
      </div>
      <span className="w-9" aria-hidden />
    </header>
    <div className="no-scrollbar flex-1 overflow-y-auto px-4 py-4 xl:px-8">
      {loading && <p className="mt-8 text-center text-sm text-black/40">正在加载该发件人的邮件…</p>}
      {!loading && error && <div role="alert" className="mx-auto mt-8 max-w-md rounded-card bg-red-50 p-4 text-sm text-red-700 shadow-lift-1">
        <p className="font-semibold">加载失败</p>
        <p className="mt-1 leading-5">{error}</p>
        <p className="mt-2 text-xs text-red-700/60">下方仍显示已加载的部分邮件。</p>
      </div>}
      {!loading && !error && messages.length === 0 && <p className="mt-8 text-center text-sm text-black/40">没有找到该发件人的邮件。</p>}
      <div role="list" aria-label={`${label} 的邮件`} className="mx-auto max-w-3xl">
        {messages.map(message => (
          <button
            key={message.id}
            role="listitem"
            onClick={() => onOpen(message)}
            className="row-lift mb-1.5 w-full rounded-card bg-white p-4 text-left hover:bg-sage/40"
          >
            <div className="flex items-start justify-between gap-3">
              <div className={`truncate text-sm ${!message.is_read ? 'font-bold' : 'font-medium text-black/70'}`}>{message.subject || '（无主题）'}</div>
              <time className="shrink-0 text-[10px] text-black/35" title={formatFullDate(message.received_at)}>{formatDate(message.received_at)}</time>
            </div>
            {!message.is_read && <span className="sr-only">未读</span>}
            <p className="mt-1.5 line-clamp-2 text-xs leading-5 text-black/45">{message.snippet || '正文尚未同步'}</p>
            <div className="mt-2 flex items-center gap-2 text-black/25">
              <span className="truncate text-[10px] text-black/30">{senderLabel(message)}</span>
              {message.has_attachments && <Paperclip size={12} />}
              {message.is_starred && <Star size={12} className="fill-amber-400 text-amber-400" />}
              {!message.is_read && <span className="ml-auto h-2 w-2 rounded-full bg-coral" aria-hidden />}
            </div>
          </button>
        ))}
      </div>
    </div>
  </>
}
