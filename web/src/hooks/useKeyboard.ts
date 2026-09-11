import { useEffect } from 'react'
import type { Message } from '../types'

// dialogOpen suppresses every shortcut while a modal is up. The handler is bound to
// window and the dialogs render as siblings of the still-mounted mailbox, so with
// focus on a dialog button 'e' archived the message behind it and 'c' mounted a
// second composer. The target check below only covers text entry, which is why the
// caller has to say whether a modal owns the screen.
export function useKeyboard(enabled: boolean, dialogOpen: boolean, messages: Message[], selected: Message | null, open: (message: Message) => void, compose: () => void, archive: () => void) {
  useEffect(() => { if (!enabled || dialogOpen) return; const handler = (event: KeyboardEvent) => { if (['INPUT', 'TEXTAREA', 'SELECT'].includes((event.target as HTMLElement).tagName)) return; const index = selected ? messages.findIndex(item => item.id === selected.id) : -1; if (event.key === 'j' && messages[index + 1]) open(messages[index + 1]); if (event.key === 'k' && messages[Math.max(0, index - 1)]) open(messages[Math.max(0, index - 1)]); if (event.key === 'c') compose(); if (event.key === 'e' && selected) archive() }; addEventListener('keydown', handler); return () => removeEventListener('keydown', handler) }, [enabled, dialogOpen, messages, selected, open, compose, archive])
}
