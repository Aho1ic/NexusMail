import { useCallback, useEffect, useState } from 'react'
import { Bell, RefreshCw, X } from 'lucide-react'
import { api } from '../lib/api'
import { decodeAddressList, formatFullDate, messageOf } from '../lib/format'
import type { Draft } from '../types'

export function OutboxDialog({ onClose, onEdit }: { onClose: () => void; onEdit: (draft: Draft) => void }) {
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
    <header className="flex items-center justify-between border-b border-black/5 px-6 py-5"><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Delivery</p><h2 className="font-serif text-3xl">草稿与发件箱</h2></div><div className="flex gap-1"><button onClick={load} aria-label="刷新" className="icon-button"><RefreshCw className={loading ? 'animate-spin' : ''} size={18} /></button><button onClick={onClose} aria-label="关闭" className="icon-button"><X size={19} /></button></div></header>
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
