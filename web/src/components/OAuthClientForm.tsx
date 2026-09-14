import { useEffect, useState } from 'react'
import { LoaderCircle } from 'lucide-react'
import { api } from '../lib/api'
import { messageOf } from '../lib/format'
import type { OAuthClientStatus } from '../types'

// A Google or Microsoft OAuth client used to be reachable only through the two
// environment variables the gateway reads at startup, which meant the one failure a
// self-hosting user actually hits — "missing Microsoft OAuth client credentials" on
// the first press of 使用网页授权 — could only be fixed by editing a file next to the
// compose file and restarting the container. The credentials can now be typed here
// and are stored encrypted, so the same repair happens in the window that reported
// the problem. The environment variables still work and still take second place: a
// row saved from this form wins, and clearing it falls back to them.
//
// The secret is write-only. The server never returns it, so there is nothing to
// prefill and an empty field cannot mean "keep the old one" — the pair is saved
// together or not at all, which is also what the server requires.
//
// This renders no <form>. AccountDialog hosts it inside the form that connects the
// mailbox, and a nested form is not valid HTML: the buttons are explicit type=button
// and submit through their own handlers.

type Props = { status: OAuthClientStatus; label: string; onSaved: (next: OAuthClientStatus) => void }

export function OAuthClientForm({ status, label, onSaved }: Props) {
  const [clientID, setClientID] = useState(status.client_id)
  const [secret, setSecret] = useState('')
  const [busy, setBusy] = useState<'save' | 'clear' | null>(null)
  const [error, setError] = useState('')
  // The parent owns the status, so a save made elsewhere — the same provider is
  // configurable from both the settings panel and the connect dialog — must not
  // leave this field showing what was typed into the other one.
  useEffect(() => { setClientID(status.client_id); setSecret('') }, [status.client_id, status.source])

  const idKey = `oauth-client-id-${status.provider}`
  const secretKey = `oauth-client-secret-${status.provider}`
  const incomplete = clientID.trim() === '' || secret.trim() === ''

  async function save() {
    setBusy('save')
    setError('')
    try {
      const next = await api.saveOAuthClient(status.provider, clientID.trim(), secret.trim())
      setClientID(next.client_id)
      setSecret('')
      onSaved(next)
    } catch (err) {
      setError(messageOf(err))
    } finally { setBusy(null) }
  }

  async function clear() {
    setBusy('clear')
    setError('')
    try {
      await api.clearOAuthClient(status.provider)
      // The server answers 204, so the state after a clear is derived rather than
      // read back: the stored row is gone, and whether an environment variable
      // stands behind it is not knowable from here. The parent re-reads.
      onSaved({ ...status, configured: false, source: 'none', client_id: '', updated_at: undefined })
    } catch (err) {
      setError(messageOf(err))
    } finally { setBusy(null) }
  }

  return <div className="mt-2.5 border-t border-black/5 pt-3">
    <label htmlFor={idKey} className="field-label">{label} Client ID</label>
    <input id={idKey} className="input font-mono text-xs" value={clientID} autoComplete="off" spellCheck={false}
      onChange={event => setClientID(event.target.value)} placeholder="123456789-abc.apps.googleusercontent.com" />
    <label htmlFor={secretKey} className="field-label">{label} Client Secret</label>
    <input id={secretKey} className="input font-mono text-xs" type="password" value={secret} autoComplete="off"
      onChange={event => setSecret(event.target.value)} placeholder={status.configured ? '已保存，不会回显；重新填写即覆盖' : '服务商后台生成的客户端密钥'} />
    <div className="mt-3 flex flex-wrap gap-2">
      <button type="button" onClick={save} disabled={busy !== null || incomplete} className="button-primary">
        {busy === 'save' && <LoaderCircle className="animate-spin" size={15} />}保存
      </button>
      {status.source === 'database' && <button type="button" onClick={clear} disabled={busy !== null} className="button-secondary">
        {busy === 'clear' && <LoaderCircle className="animate-spin" size={15} />}改回环境变量
      </button>}
    </div>
    <p className="mt-3 break-all rounded-card bg-sage/50 px-3 py-2.5 text-[11px] leading-5 text-pine">
      回调地址：<span className="font-mono">{status.redirect_uri}</span>
      <br />必须在服务商后台把这个地址原样登记为重定向 URI，否则登录后会被拒绝。
    </p>
    {status.source === 'environment' && <p className="mt-2 text-[11px] leading-5 text-black/45">
      当前使用环境变量 <span className="font-mono">{status.env_client_id_key}</span> 里的配置；在这里保存会覆盖它。
    </p>}
    {status.source !== 'environment' && <p className="mt-2 text-[11px] leading-5 text-black/45">
      也可以改用环境变量：在 docker 部署目录的 .env 里写入 <span className="font-mono">{status.env_client_id_key}</span> 与 <span className="font-mono">{status.env_client_secret_key}</span>，然后重启网关。
    </p>}
    {error && <p className="mt-2 break-words text-xs text-red-600" role="alert">{error}</p>}
  </div>
}
