import { FormEvent, useCallback, useEffect, useRef, useState } from 'react'
import { ChevronDown, ChevronLeft, ChevronRight, ExternalLink, LoaderCircle, X } from 'lucide-react'
import { APIError, api } from '../lib/api'
import { onOAuthResult } from '../lib/oauthbridge'
import { Dialog } from './shared'
import { messageOf } from '../lib/format'
import { OAuthClientForm } from './OAuthClientForm'
import { ProviderIcon, providerOptions, type ProviderOption } from './providers'
import type { OAuthClientStatus } from '../types'

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
//
// Whether the deployment holds an OAuth client for a provider is asked once on
// mount, because a dialog that offers 使用网页授权 to a gateway with no client ID
// sends the user into a consent window that comes back
// "missing Microsoft OAuth client credentials" — the failure this probe exists to
// pre-empt. A probe that fails or has not answered yet is treated as configured:
// the button is the working path for every correctly deployed gateway, and hiding
// it because a status call did not come back would break the common case to warn
// about the rare one. Only an explicit `configured: false` swaps in the form.

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
  // Keyed by backend value, so Outlook and Hotmail — one Microsoft client, two
  // chips — read the same entry. A provider missing from the map is unknown, which
  // is deliberately indistinguishable from configured at every use.
  const [clients, setClients] = useState<Record<string, OAuthClientStatus>>({})
  const [manual, setManual] = useState(false)

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

  // One read on mount. Every failure mode — no such route on an older gateway, a
  // 500, a dropped connection — lands in the same place as "not answered yet", and
  // that state renders the flow the dialog has always rendered.
  const readClients = useCallback(async () => {
    try {
      const result = await api.oauthClients()
      if (!Array.isArray(result?.items)) return
      setClients(Object.fromEntries(result.items.map(item => [item.provider, item])))
    } catch { /* unknown stays configured */ }
  }, [])

  useEffect(() => { void readClients() }, [readClients])

  // The server has its own name for "this deployment has no client for that
  // provider", and it is the only 400 a credential form fixes. The probe can miss
  // it — a client cleared from another tab, a gateway that could not answer at
  // mount — so the answer to the request itself is what re-reads the status and
  // swaps the form in, rather than leaving the user reading the English message the
  // original report was made of.
  const reportError = useCallback((err: unknown) => {
    setError(messageOf(err))
    if (err instanceof APIError && err.code === 'oauth_not_configured') void readClients()
  }, [readClients])

  function choose(option: ProviderOption) {
    setSelected(option)
    setError('')
    setManual(false)
  }

  function back() {
    popup.current?.close()
    settle()
    setSelected(null)
    setError('')
    setManual(false)
  }

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!selected) return
    // The form still wraps the client-id fields when they are shown, and Enter in
    // a field would otherwise start the popup flow that cannot work yet.
    if (selected.auth === 'oauth2' && clients[selected.backend]?.configured === false) return
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
      reportError(err)
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
            {/* The catalogue promises 网页授权 for every OAuth provider. On a gateway
                holding no client for one of them that promise is false, and the
                picker is where it is read before a click is spent on it. */}
            <span className="block text-[11px] text-black/40">{option.auth === 'oauth2' && clients[option.backend]?.configured === false ? '需先配置 OAuth Client' : option.hint}</span>
          </span>
          <ChevronRight size={17} className="shrink-0 text-black/25" />
        </button>)}
      </div>
    </div>
  </Dialog>

  const oauth = selected.auth === 'oauth2'
  const client = oauth ? clients[selected.backend] : undefined
  // Explicitly unconfigured. Unknown is not this.
  const unconfigured = client?.configured === false
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
      {oauth && !unconfigured && <div className="mt-6 rounded-card bg-sage/50 p-4 text-sm leading-6 text-pine">将打开 {selected.label} 的授权窗口，登录后自动完成连接，无需授权码。若浏览器打不开授权窗口，可用下方的「手动输入授权码」。</div>}
      {oauth && unconfigured && client && <div className="mt-6 rounded-card bg-amber-50 p-4 text-amber-900">
        <p className="text-sm font-semibold">还不能授权：缺少 {selected.label} 的 OAuth Client</p>
        <p className="mt-1.5 text-xs leading-5">先在 {selected.label} 服务商后台创建 OAuth 客户端，把 Client ID 与 Client Secret 填在下面保存；也可以写入 docker 部署目录 .env 的对应变量后重启网关。保存后即可开始授权。</p>
        <OAuthClientForm status={client} label={selected.label} onSaved={next => { setError(''); setClients(current => ({ ...current, [next.provider]: next })) }} />
      </div>}
      {error && <p className="mt-3 text-sm text-red-600" role="alert">{error}</p>}
      {(!oauth || !unconfigured) && <button disabled={busy} className="button-primary mt-7 w-full">
        {busy && <LoaderCircle className="animate-spin" size={17} />}
        {phase === 'waiting' ? '等待授权完成…' : oauth ? '使用网页授权' : '继续'}
      </button>}
      {/* The manual channel is the way out of every environment where the popup
          cannot come back: a gateway published on a host the redirect URI does not
          match, a browser that blocks the window, a consent screen finished on a
          different machine. The user carries the code across by hand. Keyed on the
          chip so switching provider cannot leave a state issued for the other one. */}
      {oauth && !unconfigured && <div className="mt-4 border-t border-black/5 pt-4">
        <button type="button" onClick={() => setManual(!manual)} aria-expanded={manual}
          className="flex w-full items-center gap-1.5 text-xs font-semibold text-pine">
          <ChevronDown size={14} className={`transition ${manual ? '' : '-rotate-90'}`} />手动输入授权码
        </button>
        {manual && <ManualCode key={selected.id} provider={selected.backend} label={selected.label} displayName={name} onCreated={onCreated} onNotConfigured={readClients} />}
      </div>}
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

// The paste-the-code path. Two server calls with a state in between: `authorize`
// issues the consent URL and a state the server will honour exactly once, and
// `code` trades the pasted value for the account. Because the state is single-use,
// any failure of the second call has spent it — so a failure drops the link and
// asks for a new one rather than letting the user retype into a state the server
// will never accept again.
//
// The whole callback URL is accepted, not just the code: the value the user can
// actually reach is the address bar of the tab the provider landed on, and picking
// `code=` out of it by hand is where this goes wrong. The server parses it.
function ManualCode({ provider, label, displayName, onCreated, onNotConfigured }: { provider: string; label: string; displayName: string; onCreated: () => void; onNotConfigured: () => void }) {
  const [link, setLink] = useState<{ url: string; state: string } | null>(null)
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState<'link' | 'submit' | null>(null)
  const [error, setError] = useState('')

  async function fetchLink() {
    setBusy('link')
    setError('')
    try {
      const result = await api.startOAuth(provider, displayName)
      setLink({ url: result.authorization_url, state: result.state })
      setCode('')
    } catch (err) {
      setError(messageOf(err))
      if (err instanceof APIError && err.code === 'oauth_not_configured') onNotConfigured()
    } finally { setBusy(null) }
  }

  async function connect() {
    if (!link || code.trim() === '') return
    setBusy('submit')
    setError('')
    try {
      await api.completeOAuth(provider, link.state, code.trim())
      onCreated()
    } catch (err) {
      // A missing client is not a spent state, and telling the user to fetch a new
      // link would send them around a loop that cannot close.
      if (err instanceof APIError && err.code === 'oauth_not_configured') {
        setError(messageOf(err))
        onNotConfigured()
        return
      }
      setLink(null)
      setError(`${messageOf(err)}（授权链接已失效，请重新获取链接后再试）`)
    } finally { setBusy(null) }
  }

  const codeKey = `oauth-manual-code-${provider}`
  return <div className="mt-3">
    <p className="text-[11px] leading-5 text-black/45">在新标签页打开下面的链接，登录 {label} 并同意授权。浏览器跳回后地址栏会带上 <span className="font-mono">code=</span>，把它后面的值复制到下面；整条回跳地址直接粘贴也可以。</p>
    {!link && <button type="button" onClick={fetchLink} disabled={busy !== null} className="button-secondary mt-3">
      {busy === 'link' && <LoaderCircle className="animate-spin" size={15} />}获取授权链接
    </button>}
    {link && <>
      <a href={link.url} target="_blank" rel="noreferrer" className="mt-3 flex items-start gap-1.5 break-all rounded-card bg-sage/50 px-3 py-2.5 font-mono text-[11px] leading-5 text-pine underline">
        <ExternalLink size={13} className="mt-0.5 shrink-0" />{link.url}
      </a>
      <label htmlFor={codeKey} className="field-label">授权码或回跳地址</label>
      {/* Enter inside this field must not submit the form around it: that form is
          the popup path, and pressing it here would open a consent window instead
          of finishing the handshake the user is already halfway through. */}
      <input id={codeKey} className="input font-mono text-xs" value={code} autoComplete="off" spellCheck={false}
        onChange={event => setCode(event.target.value)}
        onKeyDown={event => { if (event.key === 'Enter') { event.preventDefault(); void connect() } }}
        placeholder="4/0Ax4Xk… 或 http://localhost:13737/api/v1/oauth/…?code=…" />
      <div className="mt-3 flex flex-wrap gap-2">
        <button type="button" onClick={connect} disabled={busy !== null || code.trim() === ''} className="button-primary">
          {busy === 'submit' && <LoaderCircle className="animate-spin" size={15} />}完成连接
        </button>
        <button type="button" onClick={fetchLink} disabled={busy !== null} className="button-secondary">重新获取链接</button>
      </div>
    </>}
    {error && <p className="mt-2 break-words text-xs text-red-600" role="alert">{error}</p>}
  </div>
}
