import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { LoaderCircle, Paperclip, Send, X } from 'lucide-react'
import { api } from '../lib/api'
import { decodeAddressList, messageOf, splitEmails } from '../lib/format'
import type { Account, Draft, DraftInput, Message } from '../types'

type Props = { accounts: Account[]; replyTo: Message | null; initialDraft: Draft | null; onClose: () => void; onSent: () => void }

export function Composer({ accounts, replyTo, initialDraft, onClose, onSent }: Props) {
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
  // The draft is mirrored in a ref because three callers persist it — the autosave
  // timer, send and attach — and each has to see what the others already created.
  // Reading the state variable instead captured whatever value existed when the
  // closure was made: two edits two seconds apart both ran with draft === null and
  // each POSTed a new draft, so the second one silently orphaned the first.
  const draftRef = useRef<Draft | null>(initialDraft)
  const latest = useRef<DraftInput | null>(null)
  // Saves are chained rather than fired independently: a create that is still in
  // flight has not returned an id yet, so a concurrent save has nothing to update
  // and would create a second draft.
  const saving = useRef<Promise<unknown>>(Promise.resolve())
  const input: DraftInput = useMemo(() => ({ account_id: accountID, to: splitEmails(to), cc: splitEmails(cc), bcc: splitEmails(bcc), subject, body_text: body }), [accountID, to, cc, bcc, subject, body])
  latest.current = input

  // persist writes the newest input and returns the stored draft. Callers never
  // pass the draft in: whether this is a create or an update is decided at the
  // moment the turn actually runs, after any earlier save has settled.
  const persist = useCallback(async () => {
    const run = saving.current.then(async () => {
      const payload = latest.current!
      const current = draftRef.current
      const saved = current ? await api.updateDraft(current.id, current.revision, payload) : await api.createDraft(payload)
      draftRef.current = saved
      setDraft(saved)
      return saved
    })
    // Keep the chain alive after a rejection so one failed save does not wedge
    // every later one, while still surfacing the error to this caller.
    saving.current = run.catch(() => undefined)
    return run
  }, [])

  useEffect(() => {
    if (!accountID) return
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(async () => {
      try {
        setStatus('保存中…')
        const saved = await persist()
        setStatus(saved.remote_sync_state === 'synced' ? '已同步到远端' : '已保存，等待远端同步')
      } catch (err) { setStatus(messageOf(err)) }
    }, 2000)
    return () => clearTimeout(timer.current)
  }, [input, accountID, persist])

  async function send() {
    setBusy(true)
    // A pending autosave would otherwise land after the send and revive the draft.
    window.clearTimeout(timer.current)
    try {
      const saved = await persist()
      await api.sendDraft(saved.id); onSent()
    } catch (err) { setStatus(messageOf(err)); setBusy(false) }
  }
  async function attach(file?: File) {
    if (!file) return
    try {
      const saved = await persist()
      await api.uploadAttachment(saved.id, file); setStatus(`已添加 ${file.name}`)
    } catch (err) { setStatus(messageOf(err)) }
  }
  return <div className="modal-backdrop"><div className="flex h-[min(92vh,760px)] w-[min(94vw,760px)] flex-col overflow-hidden rounded-panel bg-white shadow-glass-high">
    <header className="flex items-center justify-between border-b border-black/5 px-5 py-4"><div><h2 className="font-serif text-2xl">新邮件</h2><p className="text-[10px] text-black/35">{status}</p></div><button onClick={onClose} aria-label="关闭" className="icon-button"><X size={19} /></button></header>
    <div className="flex-1 overflow-y-auto p-5">
      <select value={accountID} disabled={Boolean(draft)} onChange={event => setAccountID(Number(event.target.value))} className="input mb-2 disabled:opacity-60">{accounts.map(account => <option key={account.id} value={account.id}>{account.display_name || account.email}</option>)}</select>
      <ComposerField label="收件人" value={to} onChange={setTo} placeholder="name@example.com，多个地址用逗号分隔" />
      <div className="grid grid-cols-2 gap-2"><ComposerField label="抄送" value={cc} onChange={setCC} /><ComposerField label="密送" value={bcc} onChange={setBCC} /></div>
      <ComposerField label="主题" value={subject} onChange={setSubject} />
      <textarea value={body} onChange={event => setBody(event.target.value)} className="mt-3 min-h-[320px] w-full resize-none rounded-card border border-black/5 bg-paper/50 p-4 text-sm leading-6 outline-none focus:ring-2 focus:ring-pine/20" placeholder="写点什么…" />
    </div>
    <footer className="flex items-center justify-between border-t border-black/5 px-5 py-4"><label className="icon-button cursor-pointer"><Paperclip size={18} /><input type="file" aria-label="添加附件" className="hidden" onChange={event => attach(event.target.files?.[0])} /></label><button disabled={busy || !accountID || !to.trim()} onClick={send} className="button-primary">{busy ? <LoaderCircle className="animate-spin" size={17} /> : <Send size={17} />}发送</button></footer>
  </div></div>
}

function ComposerField({ label, value, onChange, placeholder = '' }: { label: string; value: string; onChange: (value: string) => void; placeholder?: string }) { return <label className="mt-2 flex items-center border-b border-black/5 py-2"><span className="w-16 text-xs font-semibold text-black/40">{label}</span><input value={value} onChange={event => onChange(event.target.value)} placeholder={placeholder} className="min-w-0 flex-1 bg-transparent py-1 text-sm outline-none" /></label> }
