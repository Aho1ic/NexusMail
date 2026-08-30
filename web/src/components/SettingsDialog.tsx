import { useEffect, useState } from 'react'
import { AtSign, Bell, Image, Keyboard, LogOut, Plus, X } from 'lucide-react'
import { accountStatusLabel, formatFullDate } from '../lib/format'
import { notificationPermission, requestNotificationPermission, type Preferences } from '../lib/preferences'
import type { Account } from '../types'

type Props = { preferences: Preferences; accounts: Account[]; onChange: (patch: Partial<Preferences>) => void; onClose: () => void; onAddAccount: () => void; onLogout: () => void }

export function SettingsDialog({ preferences, accounts, onChange, onClose, onAddAccount, onLogout }: Props) {
  const [permission, setPermission] = useState(notificationPermission)
  const [asking, setAsking] = useState(false)
  useEffect(() => { const handler = (event: KeyboardEvent) => { if (event.key === 'Escape') onClose() }; addEventListener('keydown', handler); return () => removeEventListener('keydown', handler) }, [onClose])
  async function askPermission() { setAsking(true); try { setPermission(await requestNotificationPermission()) } finally { setAsking(false) } }
  return <div className="modal-backdrop">
    <div role="dialog" aria-modal="true" aria-label="设置" className="flex max-h-[min(88vh,780px)] w-[min(94vw,540px)] flex-col overflow-hidden rounded-[1.7rem] bg-white shadow-2xl">
      <header className="flex shrink-0 items-center justify-between border-b border-black/5 px-7 py-6"><div><p className="text-[10px] font-bold uppercase tracking-[.2em] text-pine/40">Preferences</p><h2 className="font-serif text-3xl">设置</h2></div><button onClick={onClose} className="icon-button" aria-label="关闭设置"><X size={19} /></button></header>
      <div className="flex-1 overflow-y-auto px-7 py-2">
        <SettingsSection icon={<Bell size={14} />} title="通知">
          <SettingsToggle label="新邮件桌面通知" hint="收到新邮件时弹出系统通知，需要浏览器授权。" checked={preferences.desktopNotifications} onChange={value => onChange({ desktopNotifications: value })} />
          <SettingsToggle label="验证码通知" hint="识别到验证码时改为弹出带「复制验证码」按钮的通知，点按钮即可复制。需先开启上面的桌面通知。" checked={preferences.verificationCodeNotifications} onChange={value => onChange({ verificationCodeNotifications: value })} />
          {permission === 'default' && <div className="flex items-center justify-between gap-3 rounded-xl bg-sage/50 px-3.5 py-3 text-xs text-pine"><span>浏览器尚未授权通知。</span><button onClick={askPermission} disabled={asking} className="button-secondary shrink-0">{asking ? '请求中…' : '授权'}</button></div>}
          {permission === 'denied' && <p className="rounded-xl bg-amber-50 px-3.5 py-3 text-xs leading-5 text-amber-800">浏览器已拒绝通知权限，需在地址栏的站点设置中重新允许后才会生效。</p>}
          {permission === 'unsupported' && <p className="rounded-xl bg-black/[.03] px-3.5 py-3 text-xs leading-5 text-black/45">当前浏览器或非 HTTPS 环境不支持桌面通知。</p>}
        </SettingsSection>

        <SettingsSection icon={<Image size={14} />} title="阅读">
          <SettingsToggle label="自动加载远程图片" hint="关闭时每封邮件的外部图片都需手动加载，可避免发件人通过图片追踪你的阅读行为。" checked={preferences.autoLoadRemoteImages} onChange={value => onChange({ autoLoadRemoteImages: value })} />
        </SettingsSection>

        <SettingsSection icon={<Keyboard size={14} />} title="快捷键">
          <SettingsToggle label="启用单键快捷键" hint="关闭后 J / K / C / E 不再触发操作，可避免误触归档。" checked={preferences.keyboardShortcuts} onChange={value => onChange({ keyboardShortcuts: value })} />
          <div className={`grid grid-cols-2 gap-2 text-xs ${preferences.keyboardShortcuts ? 'text-black/55' : 'text-black/25'}`}>
            {[['J', '下一封'], ['K', '上一封'], ['C', '写邮件'], ['E', '归档当前邮件']].map(([key, label]) => <div key={key} className="flex items-center gap-2 rounded-xl bg-paper/70 px-3 py-2"><kbd>{key}</kbd>{label}</div>)}
          </div>
        </SettingsSection>

        <SettingsSection icon={<AtSign size={14} />} title="账户">
          {accounts.length === 0 && <p className="rounded-xl bg-black/[.03] px-3.5 py-3 text-xs text-black/45">还没有连接任何邮箱。</p>}
          {accounts.map(account => <div key={account.id} className="rounded-xl border border-black/5 bg-paper/60 p-3.5">
            <div className="flex items-center gap-2.5">
              <span className={`h-2 w-2 shrink-0 rounded-full ${account.status === 'connected' ? 'bg-emerald-500' : account.status === 'backoff' ? 'bg-amber-500' : 'bg-black/20'}`} />
              <span className="min-w-0 flex-1"><span className="block truncate text-sm font-semibold">{account.display_name || account.email}</span><span className="block truncate text-[11px] text-black/40">{account.email}</span></span>
              <span className="shrink-0 rounded-full bg-black/[.05] px-2 py-0.5 text-[9px] font-bold uppercase tracking-wide text-black/45">{account.provider}</span>
            </div>
            <p className="mt-2 text-[11px] text-black/35">{accountStatusLabel(account.status)}{account.last_connected_at ? ` · 最近连接 ${formatFullDate(account.last_connected_at)}` : ''}</p>
            {account.last_error && <p className="mt-2 break-words rounded-lg bg-red-50 px-2.5 py-2 text-[11px] leading-4 text-red-700" role="alert">{account.last_error}</p>}
          </div>)}
          <button onClick={onAddAccount} className="button-secondary w-full justify-center"><Plus size={15} />连接邮箱</button>
        </SettingsSection>
      </div>
      <footer className="flex shrink-0 items-center justify-between gap-3 border-t border-black/5 px-7 py-5"><button onClick={onLogout} className="flex items-center gap-2 text-xs font-semibold text-red-600 hover:text-red-700"><LogOut size={15} />退出登录</button><button onClick={onClose} className="button-primary">完成</button></footer>
    </div>
  </div>
}

function SettingsSection({ icon, title, children }: { icon: React.ReactNode; title: string; children: React.ReactNode }) {
  return <section className="border-b border-black/5 py-5 last:border-b-0">
    <h3 className="flex items-center gap-2 text-[10px] font-bold uppercase tracking-[.18em] text-pine/50">{icon}{title}</h3>
    <div className="mt-3 grid gap-2.5">{children}</div>
  </section>
}

function SettingsToggle({ label, hint, checked, onChange }: { label: string; hint: string; checked: boolean; onChange: (value: boolean) => void }) {
  return <div className="flex items-start justify-between gap-4">
    <span className="min-w-0"><span className="block text-sm font-semibold">{label}</span><span className="mt-1 block text-xs leading-5 text-black/40">{hint}</span></span>
    <button type="button" role="switch" aria-checked={checked} aria-label={label} onClick={() => onChange(!checked)} className={`relative mt-0.5 h-6 w-11 shrink-0 rounded-full transition ${checked ? 'bg-pine' : 'bg-black/15'}`}><span className={`absolute top-0.5 h-5 w-5 rounded-full bg-white shadow transition-all ${checked ? 'left-[1.375rem]' : 'left-0.5'}`} /></button>
  </div>
}
