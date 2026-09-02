import { useEffect } from 'react'
import { Archive, AtSign, Circle, Inbox, Mail, Send } from 'lucide-react'

// Dialog is the one place the four modals agree on: the backdrop, Escape, and the
// role that makes a screen reader announce them as dialogs. They had drifted —
// Escape closed the settings panel and nothing else, and only that panel carried
// role="dialog" — so the same gesture worked or did nothing depending on which
// modal was open. The panel itself stays with each caller, because their shapes
// genuinely differ: two are scroll containers with a header and footer, one is a
// form, one is a fixed-width card.
//
// Escape is safe for all four. The composer is the only one holding unsaved input
// and it autosaves every two seconds, so closing it parks a draft rather than
// discarding one.
export function Dialog({ label, onClose, className, children }: { label: string; onClose: () => void; className: string; children: React.ReactNode }) {
  useEffect(() => {
    const handler = (event: KeyboardEvent) => { if (event.key === 'Escape') onClose() }
    addEventListener('keydown', handler)
    return () => removeEventListener('keydown', handler)
  }, [onClose])
  return <div className="modal-backdrop">
    <div role="dialog" aria-modal="true" aria-label={label} className={className}>{children}</div>
  </div>
}

export function NavItem({ active, icon, label, sublabel, count, onClick }: { active: boolean; icon: React.ReactNode; label: string; sublabel?: string; count?: number; onClick: () => void }) { return <button onClick={onClick} className={`mt-1 flex w-full items-center gap-3 rounded-2xl px-3 py-3 text-left transition ${active ? 'bg-white/12 text-white shadow-lift-1' : 'text-white/65 hover:bg-white/5 hover:text-white'}`}><span className="grid w-5 place-items-center">{icon}</span><span className="min-w-0 flex-1"><span className="block truncate text-sm font-semibold">{label}</span>{sublabel && <span className="block truncate text-[9px] text-white/35">{sublabel}</span>}</span>{Boolean(count) && <span className="rounded-full bg-coral px-2 py-0.5 text-[9px] font-bold text-white">{count}</span>}</button> }
export function FolderIcon({ role }: { role: string }) { if (role === 'inbox') return <Inbox size={13} />; if (role === 'sent') return <Send size={13} />; if (role === 'archive') return <Archive size={13} />; return <Mail size={13} /> }
export function Brand({ light = false }: { light?: boolean }) { return <div className="flex items-center gap-3"><span className={`grid h-9 w-9 place-items-center rounded-2xl shadow-lift-2 ${light ? 'bg-white text-pine' : 'bg-pine text-white'}`}><AtSign size={19} strokeWidth={2.4} /></span><span className="font-serif text-xl font-semibold tracking-tight">NexusMail</span></div> }
export function Welcome({ count }: { count: number }) { return <div className="grid h-full place-items-center p-8 text-center"><div><div className="mx-auto grid h-24 w-24 place-items-center rounded-full bg-sage text-pine shadow-lift-3"><Mail size={38} strokeWidth={1.4} /></div><h2 className="mt-7 font-serif text-3xl">收件箱已就绪</h2><p className="mt-2 text-sm text-black/40">{count ? `还有 ${count} 封未读邮件等待你。` : '一切都处理好了，享受片刻清静。'}</p><div className="mx-auto mt-7 flex w-fit gap-2 text-[10px] text-black/30"><kbd>J</kbd><kbd>K</kbd> 导航 · <kbd>C</kbd> 写信 · <kbd>E</kbd> 归档</div></div></div> }
export function EmptyState() { return <div className="grid h-72 place-items-center text-center"><div><Circle className="mx-auto text-pine/25" size={38} /><p className="mt-3 font-serif text-xl">这里空空如也</p><p className="mt-1 text-xs text-black/35">换个文件夹或搜索词试试</p></div></div> }
export function Avatar({ label }: { label: string }) { return <span className="grid h-11 w-11 shrink-0 place-items-center rounded-card bg-pine font-serif text-lg text-white shadow-lift-2">{label.trim().charAt(0).toUpperCase() || '?'}</span> }
