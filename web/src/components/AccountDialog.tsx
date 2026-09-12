import { FormEvent, useCallback, useEffect, useRef, useState } from 'react'
import { ChevronLeft, ChevronRight, LoaderCircle, X } from 'lucide-react'
import { api } from '../lib/api'
import { onOAuthResult } from '../lib/oauthbridge'
import { Dialog } from './shared'
import { messageOf } from '../lib/format'
import { ProviderIcon, providerOptions, type ProviderOption } from './providers'

// Connecting a mailbox is two decisions, and they used to share one screen: a row
// of four chips over a form whose fields changed under the user depending on which
// chip was live. Picking the service is now its own step, the way every desktop
// mail client does it, so the second step only ever shows the fields that service
// actually needs. The chip row survives on step two as the way to correct a
// mis-pick without going back.
//
// The OAuth providers never render a credential field. `auth.type` is derived from
// the catalogue rather than from a list re-typed here, because posting `oauth2` for
// a password provider — or the reverse — is rejected by the accounts.auth_type
// CHECK constraint after the preset lookup has already decided the truth.

type Phase = 'idle' | 'waiting'

export function AccountDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [selected, setSelected] = useState<ProviderOption | null>(null)
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [phase, setPhase] = useState<Phase>('idle')
  const popup = useRef<Window | null>(null)

  const settle = useCallback(() => {
    popup.current = null
    setPhase('idle')
    setBusy(false)
  }, [])

  // The popup owns the outcome, so the dialog waits for a message from it. A user
  // who closes the consent window instead of finishing gets the wait cleared by
  // the `closed` poll — there is no event for it — and can try again.
  useEffect(() => {
    if (phase !== 'waiting') return
    const stop = onOAuthResult(result => {
      popup.current?.close()
      if (result.status === 'success') { settle(); onCreated(); return }
      settle()
      setError(result.reason ? `授权未完成：${result.reason}` : '授权未完成，请重试。')
    })
    const poll = window.setInterval(() => {
      if (!popup.current || !popup.current.closed) return
      settle()
      setError('授权窗口已关闭，请重试。')
    }, 500)
    return () => { stop(); window.clearInterval(poll) }
  }, [phase, onCreated, settle])

  useEffect(() => () => popup.current?.close(), [])

  function choose(option: ProviderOption) {
    setSelected(option)
    setError('')
  }

  function back() {
    popup.current?.close()
    settle()
    setSelected(null)
    setError('')
  }

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!selected) return
    setBusy(true)
    setError('')
    // The window has to be opened synchronously inside the click that triggered
    // it: a popup opened after the POST resolves is an unrequested popup as far as
    // the browser is concerned, and gets blocked. So it is opened blank and
    // navigated once the authorization URL arrives.
    if (selected.auth === 'oauth2') popup.current = window.open('', 'nexusmail-oauth', 'width=520,height=680')
    try {
      const result = await api.addAccount(selected.auth === 'oauth2'
        ? { provider: selected.backend, display_name: name, auth: { type: 'oauth2' } }
        : { provider: selected.backend, email, display_name: name, username: email, auth: { type: 'password', password } })
      if ('authorization_url' in result) {
        // A blocked popup falls back to the full-page navigation this flow used to
        // do unconditionally: the handshake still completes, it just costs the
        // open mailbox.
        if (!popup.current) { location.href = result.authorization_url; return }
        popup.current.location.href = result.authorization_url
        setPhase('waiting')
        return
      }
      settle()
      onCreated()
    } catch (err) {
      popup.current?.close()
      settle()
      setError(messageOf(err))
    }
  }

  const panel = 'w-[min(92vw,460px)] rounded-panel bg-white shadow-glass-high'
  if (!selected) return <Dialog label="连接邮箱" onClose={onClose} className={panel}>
    <div className="p-7">
      <Header onClose={onClose} title="连接邮箱" />
      <p className="mt-5 text-sm text-black/45">选择邮箱服务商</p>
      <div className="mt-3 space-y-2">
        {providerOptions.map(option => <button
          type="button"
          key={option.id}
          onClick={() => choose(option)}
          className="card-lift flex w-full items-center gap-3.5 rounded-card border border-black/5 bg-paper/60 px-4 py-3.5 text-left hover:bg-white"
        >
          <ProviderIcon id={option.id} size={22} />
          <span className="min-w-0 flex-1">
            <span className="block text-sm font-semibold">{option.label}</span>
            <span className="block text-[11px] text-black/40">{option.hint}</span>
          </span>
          <ChevronRight size={17} className="shrink-0 text-black/25" />
        </button>)}
      </div>
    </div>
  </Dialog>

  const oauth = selected.auth === 'oauth2'
  return <Dialog label="连接邮箱" onClose={onClose} className={panel}>
    <form onSubmit={submit} className="p-7">
      <div className="flex items-center gap-2">
        <button type="button" aria-label="返回" onClick={back} className="icon-button shrink-0"><ChevronLeft size={19} /></button>
        <Header onClose={onClose} title={`连接 ${selected.label}`} />
      </div>
      {/* Equal columns while they fit, a horizontal scroller once they do not: the
          full row at a legible size does not survive a 320px viewport, and wrapping
          to a second row costs the vertical space this row exists to save. */}
      <div className="no-scrollbar mt-6 flex gap-2 overflow-x-auto pb-1">
        {providerOptions.map(option => <button
          type="button"
          key={option.id}
          onClick={() => choose(option)}
          aria-pressed={option.id === selected.id}
          className={`flex min-w-[76px] flex-1 shrink-0 flex-col items-center gap-1.5 rounded-2xl border px-2 py-2.5 text-[11px] font-semibold transition ${option.id === selected.id ? 'border-pine bg-sage text-pine shadow-lift-2' : 'border-black/5 text-black/45 hover:border-black/10 hover:text-black/65'}`}
        >
          <ProviderIcon id={option.id} />
          {option.label}
        </button>)}
      </div>
      <label htmlFor="account-name" className="field-label">显示名称</label>
      <input id="account-name" className="input" value={name} onChange={e => setName(e.target.value)} placeholder="工作邮箱" />
      {!oauth && <>
        <label htmlFor="account-email" className="field-label">邮箱地址</label>
        <input id="account-email" className="input" type="email" required value={email} onChange={e => setEmail(e.target.value)} />
        <label htmlFor="account-password" className="field-label">{selected.credential}</label>
        <input id="account-password" className="input" type="password" required value={password} onChange={e => setPassword(e.target.value)} />
        {/* The space before the interpolation is deliberate: the credential name can
            start with Latin script ("App 专用密码"), and 生成的App reads as one run
            without it. */}
        <p className="mt-2 text-xs text-black/40">请使用邮箱服务商生成的 {selected.credential}，而非网页登录密码。</p>
      </>}
      {oauth && <div className="mt-6 rounded-card bg-sage/50 p-4 text-sm leading-6 text-pine">将打开 {selected.label} 的授权窗口，登录后自动完成连接，无需授权码。部署者必须已配置对应 OAuth Client ID 与 Secret。</div>}
      {error && <p className="mt-3 text-sm text-red-600" role="alert">{error}</p>}
      <button disabled={busy} className="button-primary mt-7 w-full">
        {busy && <LoaderCircle className="animate-spin" size={17} />}
        {phase === 'waiting' ? '等待授权完成…' : oauth ? '使用网页授权' : '继续'}
      </button>
    </form>
  </Dialog>
}

function Header({ title, onClose }: { title: string; onClose: () => void }) {
  return <div className="flex flex-1 items-center justify-between gap-3">
    <div className="min-w-0">
      <p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Connect</p>
      <h2 className="truncate font-serif text-3xl">{title}</h2>
    </div>
    <button type="button" aria-label="关闭" onClick={onClose} className="icon-button shrink-0"><X size={19} /></button>
  </div>
}
