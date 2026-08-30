import { useEffect, useMemo, useState } from 'react'
import { Archive, ChevronDown, Copy, File, LoaderCircle, SquarePen, Star } from 'lucide-react'
import { decodeEncodedWords, displaySender, formatBytes, formatFullDate } from '../lib/format'
import { messageDocument, prepareMessageHTML } from '../lib/messagehtml'
import { copyText } from '../lib/notifications'
import type { Message, MessageDetails } from '../types'
import { Avatar } from './shared'

type Props = {
  selected: Message
  details: MessageDetails | null
  autoLoadRemoteImages: boolean
  onBack: () => void
  onStar: () => void
  onArchive: () => void
  onReply: () => void
  onNotice: (text: string) => void
}

export function MessageDetail({ selected, details, autoLoadRemoteImages, onBack, onStar, onArchive, onReply, onNotice }: Props) {
  const message = details?.message ?? selected
  const bodyHTML = message.body_html ?? ''
  const [loadRemoteImages, setLoadRemoteImages] = useState(autoLoadRemoteImages)
  // Remote images are re-blocked per message unless the setting opts in, so
  // trusting one sender never silently leaks the next sender a read receipt.
  useEffect(() => setLoadRemoteImages(autoLoadRemoteImages), [message.id, autoLoadRemoteImages])
  const renderedHTML = useMemo(() => prepareMessageHTML(bodyHTML, details?.attachments ?? [], message.id, loadRemoteImages), [bodyHTML, details?.attachments, message.id, loadRemoteImages])
  const hasRemoteImages = bodyHTML.includes('data-nexusmail-remote-src')
  // The chip is both the Safari path (no notification buttons there) and the way
  // back to a code whose notification was dismissed or missed on a reconnect.
  const otpCode = details?.otp_code ?? ''
  async function copyCode() {
    const copied = await copyText(otpCode)
    onNotice(copied ? `已复制验证码 ${otpCode}` : `复制失败，验证码为 ${otpCode}`)
  }
  return <>
    <header className="flex items-center justify-between border-b border-black/5 px-5 py-4"><button onClick={onBack} className="md:hidden icon-button"><ChevronDown className="rotate-90" size={19} /></button><div className="flex gap-1"><button onClick={onArchive} className="icon-button" title="归档 (e)"><Archive size={18} /></button><button onClick={onStar} className="icon-button" title="星标"><Star size={18} className={message.is_starred ? 'fill-amber-400 text-amber-400' : ''} /></button></div><button onClick={onReply} className="button-secondary"><SquarePen size={16} />回复</button></header>
    <article className="flex-1 overflow-y-auto px-6 py-8 lg:px-12 xl:px-16">
      <div className="mx-auto max-w-3xl"><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">{formatFullDate(message.received_at)}</p><h1 className="mt-3 font-serif text-3xl leading-tight lg:text-4xl">{message.subject || '（无主题）'}</h1>
        <div className="mt-7 flex items-center gap-3 border-b border-black/5 pb-6"><Avatar label={displaySender(message.sender)} /><div className="min-w-0"><div className="truncate font-serif text-lg">{displaySender(message.sender)}</div><div className="truncate text-xs text-black/40">发给 {decodeEncodedWords(message.recipients) || '我'}</div></div></div>
        {otpCode && <button onClick={copyCode} className="mt-6 flex items-center gap-3 rounded-2xl bg-sage/60 px-4 py-3 text-left transition hover:bg-sage" aria-label={`复制验证码 ${otpCode}`}><span className="grid h-9 w-9 place-items-center rounded-xl bg-white/70 text-pine"><Copy size={16} /></span><span><span className="block font-mono text-lg font-bold tracking-[.18em] text-pine">{otpCode}</span><span className="text-[11px] text-pine/55">检测到验证码，点击复制</span></span></button>}
        {!details && <div className="grid h-52 place-items-center"><LoaderCircle className="animate-spin text-pine/40" /></div>}
        {details && message.body_state === 'error' && <div className="my-10 rounded-2xl bg-amber-50 p-5 text-sm text-amber-800">正文获取失败，下次账号同步时会自动重试。</div>}
        {details && message.body_state !== 'ready' && message.body_state !== 'error' && <div className="my-10 rounded-2xl bg-sage/50 p-5 text-sm text-pine">正文正在从邮件服务商异步获取，稍后会自动刷新。</div>}
        {details && hasRemoteImages && !loadRemoteImages && <button onClick={() => setLoadRemoteImages(true)} className="mt-6 rounded-xl bg-amber-50 px-4 py-2 text-xs font-semibold text-amber-800">本邮件包含已阻止的远程图片，点击临时加载</button>}
        {details && message.body_html ? <iframe title="邮件正文" sandbox="allow-popups allow-popups-to-escape-sandbox" referrerPolicy="no-referrer" srcDoc={messageDocument(renderedHTML)} className="mt-8 min-h-[520px] w-full border-0" /> : details && <pre className="mt-8 whitespace-pre-wrap font-sans text-[15px] leading-7 text-black/75">{message.body_text || message.snippet}</pre>}
        {!!details?.attachments?.length && <div className="mt-10 border-t border-black/5 pt-5"><h2 className="text-xs font-bold uppercase tracking-wider text-black/40">附件</h2><div className="mt-3 grid gap-2 sm:grid-cols-2">{details.attachments.map(att => <a key={att.id} href={`/api/v1/messages/${message.id}/attachments/${att.id}`} className="flex items-center gap-3 rounded-xl border border-black/5 p-3 hover:bg-paper"><span className="grid h-9 w-9 place-items-center rounded-lg bg-sage"><File size={17} /></span><span className="min-w-0"><span className="block truncate text-xs font-semibold">{att.filename}</span><span className="text-[10px] text-black/35">{formatBytes(att.size_bytes)}</span></span></a>)}</div></div>}
      </div>
    </article>
  </>
}
