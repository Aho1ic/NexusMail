import { Inbox, LogOut, Plus, Send, Settings, SquarePen } from 'lucide-react'
import type { Account, Mailbox } from '../types'
import { Brand, FolderIcon, NavItem } from './shared'

type Props = {
  visible: boolean
  accounts: Account[]
  mailboxes: Mailbox[]
  selectedAccount: number | null
  selectedMailbox: number | null
  unreadCount: number
  onCompose: () => void
  onSelectAll: () => void
  onSelectAccount: (id: number) => void
  onSelectMailbox: (id: number) => void
  onShowOutbox: () => void
  onShowAccounts: () => void
  onShowSettings: () => void
  onLogout: () => void
}

export function MailboxNav({ visible, accounts, mailboxes, selectedAccount, selectedMailbox, unreadCount, onCompose, onSelectAll, onSelectAccount, onSelectMailbox, onShowOutbox, onShowAccounts, onShowSettings, onLogout }: Props) {
  return <aside className={`${visible ? 'flex' : 'hidden'} md:flex w-full md:w-[260px] shrink-0 flex-col bg-pine text-white`}>
    <div className="p-6"><Brand light /></div>
    <button onClick={onCompose} className="mx-5 mt-3 flex items-center justify-center gap-2 rounded-2xl bg-coral px-5 py-3.5 text-sm font-semibold shadow-lg shadow-black/10 transition hover:-translate-y-0.5"><SquarePen size={18} />写邮件</button>
    <nav className="mt-8 flex-1 overflow-y-auto px-3">
      <NavItem active={!selectedAccount && !selectedMailbox} icon={<Inbox size={18} />} label="All Inboxes" count={unreadCount} onClick={onSelectAll} />
      <NavItem active={false} icon={<Send size={18} />} label="草稿与发件箱" onClick={onShowOutbox} />
      <div className="mt-7 px-3 text-[10px] font-bold uppercase tracking-[.22em] text-white/40">账户</div>
      {accounts.map(account => <div key={account.id}>
        <NavItem active={selectedAccount === account.id && !selectedMailbox} icon={<span className={`h-2.5 w-2.5 rounded-full ${account.status === 'connected' ? 'bg-emerald-300' : account.status === 'backoff' ? 'bg-amber-300' : 'bg-white/30'}`} />} label={account.display_name || account.email} sublabel={account.email} onClick={() => onSelectAccount(account.id)} />
        {account.last_error && <p className="mx-3 mt-1 break-words rounded-lg bg-red-400/10 px-2 py-1.5 text-[10px] leading-4 text-red-200" role="alert">同步失败：{account.last_error}</p>}
        {selectedAccount === account.id && mailboxes.map(box => <button key={box.id} onClick={() => onSelectMailbox(box.id)} className={`ml-8 flex w-[calc(100%-2.5rem)] items-center gap-2 rounded-xl px-3 py-2 text-left text-xs ${selectedMailbox === box.id ? 'bg-white/12 text-white' : 'text-white/50 hover:text-white'}`}><FolderIcon role={box.role} />{box.display_name}</button>)}
      </div>)}
      <button onClick={onShowAccounts} className="mt-4 flex w-full items-center gap-3 rounded-xl px-3 py-3 text-sm text-white/50 hover:bg-white/5 hover:text-white"><Plus size={18} />连接邮箱</button>
    </nav>
    <div className="flex items-center justify-between border-t border-white/10 p-5 text-white/50"><button onClick={onShowSettings} className="flex items-center gap-2 text-xs hover:text-white" title="设置"><Settings size={18} />设置</button><button onClick={onLogout} className="flex items-center gap-2 text-xs hover:text-white"><LogOut size={16} />退出</button></div>
  </aside>
}
