import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AccountDialog } from './components/AccountDialog'
import { Composer } from './components/Composer'
import { OutboxDialog } from './components/OutboxDialog'
import { SettingsDialog } from './components/SettingsDialog'
import { defaultPreferences } from './lib/preferences'

// The four modals had drifted apart on the two things a modal owes the user:
// Escape closed the settings panel and nothing else, and only that panel carried
// role="dialog", so a screen reader announced the other three as plain content.
// Both now come from one shared Dialog, and this asserts every caller through it —
// the drift was invisible per-component and only shows when they are compared.

vi.mock('./lib/api', () => ({
  APIError: class extends Error {},
  api: {
    drafts: vi.fn(async () => ({ items: [] })),
    draft: vi.fn(async () => ({ draft: null, attachments: [] })),
  },
  isAuthenticated: () => true,
}))

const accounts = [{ id: 1, email: 'a@example.com', display_name: 'A', provider: 'qq', status: 'connected' }]

// Rendered per case rather than in a table of elements, because two of them fetch
// on mount and would leak between cases.
const dialogs: Record<string, (onClose: () => void) => React.ReactElement> = {
  '连接邮箱': onClose => <AccountDialog onClose={onClose} onCreated={() => {}} />,
  '草稿与发件箱': onClose => <OutboxDialog onClose={onClose} onEdit={() => {}} />,
  '新邮件': onClose => <Composer accounts={accounts} replyTo={null} initialDraft={null} onClose={onClose} onSent={() => {}} />,
  '设置': onClose => <SettingsDialog preferences={defaultPreferences} accounts={[]} onChange={() => {}} onClose={onClose} onAddAccount={() => {}} onLogout={() => {}} />,
}

describe('dialog behaviour is the same for every modal', () => {
  beforeEach(() => { localStorage.clear() })
  afterEach(cleanup)

  for (const [label, renderDialog] of Object.entries(dialogs)) {
    it(`closes ${label} on Escape`, () => {
      const onClose = vi.fn()
      render(renderDialog(onClose))
      fireEvent.keyDown(window, { key: 'Escape' })
      expect(onClose).toHaveBeenCalledTimes(1)
    })

    it(`announces ${label} as a modal dialog`, () => {
      render(renderDialog(() => {}))
      const dialog = screen.getByRole('dialog')
      expect(dialog).toHaveAttribute('aria-modal', 'true')
      expect(dialog).toHaveAttribute('aria-label', label)
    })

    it(`leaves ${label} open on an unrelated key`, () => {
      const onClose = vi.fn()
      render(renderDialog(onClose))
      fireEvent.keyDown(window, { key: 'a' })
      expect(onClose).not.toHaveBeenCalled()
    })
  }

  // The listener is bound to window, so a dialog that unmounts without removing it
  // keeps answering Escape for whatever is on screen next.
  it('stops answering Escape once unmounted', () => {
    const onClose = vi.fn()
    const view = render(<SettingsDialog preferences={defaultPreferences} accounts={[]} onChange={() => {}} onClose={onClose} onAddAccount={() => {}} onLogout={() => {}} />)
    view.unmount()
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()
  })
})
