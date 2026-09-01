import { FormEvent, useState } from 'react'
import { LoaderCircle } from 'lucide-react'
import { api } from '../lib/api'
import { messageOf } from '../lib/format'
import { Brand } from './shared'

export function Login({ onAuthenticated }: { onAuthenticated: () => void }) {
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
  return <main className="app-stage relative grid min-h-screen place-items-center overflow-hidden text-ink">
    <div aria-hidden className="pointer-events-none absolute -right-24 -top-32 h-96 w-96 rounded-full bg-sage opacity-80 blur-3xl" />
    <div aria-hidden className="pointer-events-none absolute -bottom-36 -left-24 h-96 w-96 rounded-full bg-coral/25 blur-3xl" />
    <form onSubmit={submit} className="relative w-[min(92vw,420px)] rounded-shell border border-white/60 bg-white/70 p-9 shadow-stage backdrop-blur-2xl">
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
