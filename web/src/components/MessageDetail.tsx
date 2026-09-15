import { useEffect, useMemo, useRef, useState } from 'react'
import { Archive, CalendarPlus, ChevronDown, Copy, File, FileDown, Languages, LoaderCircle, MoreVertical, Share2, SquarePen, Star, ShieldAlert } from 'lucide-react'
import { decodeEncodedWords, displaySender, formatBytes, formatFullDate, messageOf } from '../lib/format'
import { inlineImageRefs, messageDocument, prepareMessageHTML } from '../lib/messagehtml'
import { copyText } from '../lib/notifications'
import { buildCalendarICS, exportMessagePDF, isMacLike, openSystemCalendar, shareMessage } from '../lib/messageActions'
import { api, APIError } from '../lib/api'
import type { Message, MessageDetails } from '../types'
import { Avatar } from './shared'

type Props = {
  selected: Message
  details: MessageDetails | null
  loadError?: string
  autoLoadRemoteImages: boolean
  onBack: () => void
  onStar: () => void
  onArchive: () => void
  onJunk?: () => void
  onReply: () => void
  onNotice: (text: string) => void
}

// noInlineSources is a shared empty map so the pre-resolution render and the
// message-change reset both settle on one identity, which lets setState bail out
// instead of scheduling a render that changes nothing.
const noInlineSources: Map<string, string> = new Map()

export function MessageDetail({ selected, details, loadError, autoLoadRemoteImages, onBack, onStar, onArchive, onJunk, onReply, onNotice }: Props) {
  const message = details?.message ?? selected
  const bodyHTML = message.body_html ?? ''
  const [loadRemoteImages, setLoadRemoteImages] = useState(autoLoadRemoteImages)
  const [menuOpen, setMenuOpen] = useState(false)
  const [translation, setTranslation] = useState<{ id: number; text: string } | null>(null)
  const [translating, setTranslating] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const moreButtonRef = useRef<HTMLButtonElement>(null)
  // Remote images are re-blocked per message unless the setting opts in, so
  // trusting one sender never silently leaks the next sender a read receipt.
  useEffect(() => setLoadRemoteImages(autoLoadRemoteImages), [message.id, autoLoadRemoteImages])
  useEffect(() => { setMenuOpen(false); setTranslation(null); setTranslating(false) }, [message.id])
  useEffect(() => {
    if (!menuOpen) {
      moreButtonRef.current?.focus()
      return
    }
    const items = () => Array.from(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])
    items()[0]?.focus()
    const onDown = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) setMenuOpen(false)
    }
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        setMenuOpen(false)
        moreButtonRef.current?.focus()
        return
      }
      if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp' && event.key !== 'Home' && event.key !== 'End') return
      const list = items()
      if (list.length === 0) return
      event.preventDefault()
      const index = list.indexOf(document.activeElement as HTMLElement)
      if (event.key === 'Home') list[0].focus()
      else if (event.key === 'End') list[list.length - 1].focus()
      else if (event.key === 'ArrowDown') list[(index + 1 + list.length) % list.length].focus()
      else list[(index - 1 + list.length) % list.length].focus()
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [menuOpen])
  const [inlineSources, setInlineSources] = useState(noInlineSources)
  const attachments = details?.attachments
  const inlineRefs = useMemo(() => inlineImageRefs(bodyHTML, attachments ?? [], message.id), [bodyHTML, attachments, message.id])
  // The frame cannot fetch its own inline parts — see prepareMessageHTML — so the
  // parent fetches them with its credentials and the bytes are substituted into the
  // document before it is handed to srcDoc. Nothing is retained across messages:
  // the map is dropped on every change and the request is aborted, so reading mail
  // does not accumulate one resolved part per inline image per message.
  useEffect(() => {
    setInlineSources(noInlineSources)
    if (inlineRefs.length === 0) return
    const abort = new AbortController()
    let live = true
    void (async () => {
      const resolved = await Promise.all(inlineRefs.map(async ref => {
        try {
          const response = await fetch(ref.url, { credentials: 'same-origin', signal: abort.signal })
          if (!response.ok) return null
          const blob = await response.blob()
          // FileReader rather than a hand-rolled base64 pass: it takes arbitrary
          // binary without a per-byte String.fromCharCode loop, and carries the
          // part's own content type into the URL.
          const source = await new Promise<string>((resolve, reject) => {
            const reader = new FileReader()
            reader.onerror = () => reject(reader.error)
            reader.onload = () => resolve(String(reader.result))
            reader.readAsDataURL(blob)
          })
          return [ref.contentID, source] as const
        } catch {
          // A part that cannot be fetched — 4xx, a transport failure, or the abort
          // when the message changes — falls back to the blocked-image presentation,
          // the same placeholder a blocked remote image gets.
          return null
        }
      }))
      if (!live) return
      const next = new Map(resolved.filter((entry): entry is readonly [string, string] => entry !== null))
      if (next.size > 0) setInlineSources(next)
    })()
    return () => { live = false; abort.abort() }
  }, [inlineRefs])
  const renderedHTML = useMemo(() => prepareMessageHTML(bodyHTML, inlineSources, loadRemoteImages), [bodyHTML, inlineSources, loadRemoteImages])
  const hasRemoteImages = bodyHTML.includes('data-nexusmail-remote-src')
  // The chip is both the Safari path (no notification buttons there) and the way
  // back to a code whose notification was dismissed or missed on a reconnect.
  const otpCode = details?.otp_code ?? ''
  async function copyCode() {
    const copied = await copyText(otpCode)
    onNotice(copied ? `已复制验证码 ${otpCode}` : `复制失败，验证码为 ${otpCode}`)
  }
  async function handleShare() {
    setMenuOpen(false)
    onNotice(await shareMessage(message) || '已取消分享')
  }
  function handlePDF() {
    setMenuOpen(false)
    onNotice(exportMessagePDF(message, details))
  }
  async function handleCalendar() {
    setMenuOpen(false)
    const built = buildCalendarICS(message, details)
    if (!built) { onNotice('未能从邮件中识别时间，请手动创建日程'); return }
    onNotice(await openSystemCalendar(message, details))
  }
  async function handleTranslate() {
    setMenuOpen(false)
    if (translating) return
    setTranslating(true)
    try {
      const result = await api.translateMessage(message.id, 'zh-CN')
      setTranslation({ id: message.id, text: result.text })
      onNotice('已翻译为中文')
    } catch (err) {
      if (err instanceof APIError && err.code === 'translate_not_configured') {
        onNotice('未配置翻译服务：请在网关设置 NEXUSMAIL_TRANSLATE_URL')
      } else {
        onNotice(`翻译失败：${messageOf(err)}`)
      }
    } finally { setTranslating(false) }
  }
  function handleJunk() {
    setMenuOpen(false)
    onJunk?.()
  }
  return <>
    {/* pb is tighter than pt so the divider sits closer to the toolbar, and the
        menu's top-full lands on that border rather than floating above it. */}
    <header ref={menuRef} className="relative flex items-center justify-between border-b border-black/10 px-5 pb-2 pt-3.5">
      <button onClick={onBack} aria-label="返回列表" className="md:hidden icon-button"><ChevronDown className="rotate-90" size={19} /></button>
      <div className="flex gap-1">
        <button onClick={onArchive} className="icon-button" title="归档 (e)"><Archive size={18} /></button>
        <button onClick={onStar} className="icon-button" title="星标"><Star size={18} className={message.is_starred ? 'fill-amber-400 text-amber-400' : ''} /></button>
        <button ref={moreButtonRef} onClick={() => setMenuOpen(open => !open)} className={`icon-button ${menuOpen ? 'bg-sage text-pine' : ''}`} aria-label="更多操作" aria-haspopup="menu" aria-expanded={menuOpen} title="更多操作"><MoreVertical size={18} /></button>
      </div>
      <button onClick={onReply} className="button-secondary"><SquarePen size={16} />回复</button>
      {menuOpen && <div role="menu" className="menu-pop absolute left-5 top-full z-40 w-[13.5rem]">
        <MenuButton icon={<Share2 size={15} />} label="分享" onClick={handleShare} />
        {isMacLike() && <MenuButton icon={<CalendarPlus size={15} />} label="添加到日历" onClick={handleCalendar} />}
        <MenuButton icon={<FileDown size={15} />} label="导出 PDF" onClick={handlePDF} />
        <MenuButton icon={<Languages size={15} />} label={translating ? '翻译中…' : '一键翻译'} onClick={handleTranslate} />
        <div className="border-t border-black/5" />
        <MenuButton icon={<ShieldAlert size={15} />} label="举报垃圾邮件" danger onClick={handleJunk} />
      </div>}
    </header>
    {/* no-scrollbar: the reading pane is the one place the wheel is the primary
        navigation, and a permanent grey bar down the edge of every mail bought
        nothing — the wheel, J/K and touch all scroll without it. */}
    {/* The padding ramp starts at xl, not lg. At lg it was taking 96px from a pane
        that only had 266px, so a third of the reading width went to margin at the
        one width where the article could least afford it. */}
    <article className="no-scrollbar flex-1 overflow-y-auto px-6 py-8 xl:px-12 2xl:px-16">
      {/* The column is capped at a reading measure and centred rather than given
          whatever the pane offers, because the two are far apart once the display is
          large. At 2560 — a 27-inch screen at its native logical size — the nav and
          the list column leave the pane 1802px, and mail does not come in that width:
          a newsletter is 750-860px and a line of prose is capped at 768px below. With
          the old 1600px cap the message therefore sat against the left edge with
          roughly 900px of empty white to its right, and the subject — capped at 5xl
          of its own — stopped 576px short of the column it sat in.
          1024 is the measure the subject used to carry alone at 2xl, so it can hand
          that job to the column: subject, sender row, prose and body frame now end on
          the same edge, and the width the pane has left over is split evenly between
          two margins instead of being heaped on the right. The cap only ever binds
          above a ~2470px viewport — at 1920 the pane's own 1034px column is already
          under it — so nothing changes for the screens the layout was tuned on.
          It has to stay above what real mail is built for: the frame keeps the full
          column, and .nexusmail-scroll is what sends anything wider than it into a
          scrollbar of its own instead of dragging the whole pane sideways.
          It is also a full-height flex column, which is what lets the body frame below
          claim the height the header and attachments do not use. min-h-full rather than
          h-full: mail taller than the pane still grows the column and scrolls the
          article, exactly as before. */}
      {/* break-words is what stops a subject from leaving the column: a bracketed
          order number or a commit hash is one unbreakable token, and without it a
          1280px window pushed 【3619484002602023】 straight through the right edge.
          text-balance evens the lines instead of leaving one word alone on the last.
          The size ramp waits for xl for the same reason as the padding — 36px type in
          a 266px column wrapped every four characters. */}
      <div className="mx-auto flex min-h-full w-full max-w-5xl flex-col"><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">{formatFullDate(message.received_at)}</p><h1 className="mt-3 break-words font-serif text-3xl leading-tight text-balance xl:text-4xl">{message.subject || '（无主题）'}</h1>
        <div className="mt-7 flex items-center gap-3 border-b border-black/5 pb-6"><Avatar label={displaySender(message.sender)} /><div className="min-w-0"><div className="truncate font-serif text-lg">{displaySender(message.sender)}</div><div className="truncate text-xs text-black/40">发给 {decodeEncodedWords(message.recipients) || '我'}</div></div></div>
        {otpCode && <button onClick={copyCode} className="card-lift mt-6 flex items-center gap-3 rounded-card bg-sage/60 px-4 py-3 text-left hover:bg-sage" aria-label={`复制验证码 ${otpCode}`}><span className="grid h-9 w-9 place-items-center rounded-2xl bg-white/70 text-pine"><Copy size={16} /></span><span><span className="block font-mono text-lg font-bold tracking-[.18em] text-pine">{otpCode}</span><span className="text-[11px] text-pine/55">检测到验证码，点击复制</span></span></button>}
        {translation && translation.id === message.id && <div className="mt-6 max-w-3xl rounded-card border border-pine/10 bg-sage/40 p-4">
          <div className="mb-2 flex items-center justify-between gap-2">
            <span className="text-[10px] font-bold uppercase tracking-[.18em] text-pine/50">译文 · 中文</span>
            <button onClick={() => setTranslation(null)} className="text-[11px] text-black/40 hover:text-black/70">关闭</button>
          </div>
          <p className="whitespace-pre-wrap text-sm leading-6 text-black/75">{translation.text}</p>
        </div>}
        {!details && !loadError && <div className="grid h-52 place-items-center"><LoaderCircle className="animate-spin text-pine/40" /></div>}
        {!details && loadError && <div role="alert" className="my-10 max-w-3xl rounded-card bg-red-50 p-5 text-sm text-red-700 shadow-lift-1">
          <p className="font-semibold">正文加载失败</p>
          <p className="mt-1.5 leading-5 text-red-700/80">{loadError}</p>
          <p className="mt-2 text-xs text-red-700/60">关闭后重新打开这封邮件可以再试一次。</p>
        </div>}
        {/* Not "will retry on the next sync": the prefetch gives up on a body after
            maxBodyAttempts and stops enqueueing it for the life of the process, so that
            promise expires. Reopening does retry — the foreground fetch is not capped —
            and it is the only move left once the prefetch has written this state. */}
        {details && message.body_state === 'error' && <div className="my-10 max-w-3xl rounded-card bg-amber-50 p-5 text-sm text-amber-800 shadow-lift-1">正文获取失败，重新打开这封邮件可以再试一次。</div>}
        {details && message.body_state !== 'ready' && message.body_state !== 'error' && <div className="my-10 max-w-3xl rounded-card bg-sage/50 p-5 text-sm text-pine shadow-lift-1">正文正在从邮件服务商异步获取，稍后会自动刷新。</div>}
        {details && hasRemoteImages && !loadRemoteImages && <button onClick={() => setLoadRemoteImages(true)} className="card-lift mt-6 rounded-2xl bg-amber-50 px-4 py-2 text-xs font-semibold text-amber-800">本邮件包含已阻止的远程图片，点击临时加载</button>}
        {/* Only the HTML frame takes the full column: the sender controls that layout
            and needs the room. Plain text wraps to whatever it is given, so it keeps a
            measure of its own — inheriting the column would set a 1024px line.
            flex-1 is what fixes the frame that used to render as a 520px band with the
            rest of a 1300px pane blank underneath it. The frame cannot size itself to
            its content — it is a scriptless sandbox, so nothing on either side may
            measure the document — so it takes the pane's leftover height and scrolls
            internally.
            The floor drops at md, where the pane is a column of its own. 520px there
            was larger than the height a 1440x900 pane has left to give (469px), so the
            floor pushed the article into an outer scrollbar to honour a minimum the
            screen could have filled by itself. 200px only guards the degenerate case —
            a window short enough that the header eats the column and flex-basis 0
            would otherwise leave the frame nothing. Below md the taller floor stays:
            a phone scrolls the article, and a frame that scrolls inside a scrolling
            page is worse on touch than a long page. */}
        {details && message.body_html ? <iframe title="邮件正文" sandbox="allow-popups allow-popups-to-escape-sandbox" referrerPolicy="no-referrer" srcDoc={messageDocument(renderedHTML)} className="mt-8 min-h-[520px] w-full flex-1 border-0 md:min-h-[200px]" /> : details && <pre className="mt-8 max-w-3xl whitespace-pre-wrap font-sans text-[15px] leading-7 text-black/75">{message.body_text || message.snippet}</pre>}
        {/* A 1024px column fits four chips at 2xl, so the grid gains columns instead
            of stretching the ones it has. */}
        {!!details?.attachments?.length && <div className="mt-10 border-t border-black/5 pt-5"><h2 className="text-xs font-bold uppercase tracking-wider text-black/40">附件</h2><div className="mt-3 grid gap-2 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">{details.attachments.map(att => <a key={att.id} href={`/api/v1/messages/${message.id}/attachments/${att.id}`} className="card-lift flex items-center gap-3 rounded-card border border-black/5 bg-white p-3 hover:bg-paper"><span className="grid h-9 w-9 place-items-center rounded-xl bg-sage"><File size={17} /></span><span className="min-w-0"><span className="block truncate text-xs font-semibold">{att.filename}</span><span className="text-[10px] text-black/35">{formatBytes(att.size_bytes)}</span></span></a>)}</div></div>}
      </div>
    </article>
  </>
}

function MenuButton({ icon, label, onClick, danger }: { icon: React.ReactNode; label: string; onClick: () => void; danger?: boolean }) {
  return <button role="menuitem" type="button" onClick={onClick} className={danger ? 'menu-item-danger' : 'menu-item'}>{icon}{label}</button>
}
