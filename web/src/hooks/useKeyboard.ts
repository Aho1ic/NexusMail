import { useEffect } from 'react'
import type { Message } from '../types'

export function useKeyboard(enabled: boolean, messages: Message[], selected: Message | null, open: (message: Message) => void, compose: () => void, archive: () => void) {
  useEffect(() => { if (!enabled) return; const handler = (event: KeyboardEvent) => { if (['INPUT', 'TEXTAREA', 'SELECT'].includes((event.target as HTMLElement).tagName)) return; const index = selected ? messages.findIndex(item => item.id === selected.id) : -1; if (event.key === 'j' && messages[index + 1]) open(messages[index + 1]); if (event.key === 'k' && messages[Math.max(0, index - 1)]) open(messages[Math.max(0, index - 1)]); if (event.key === 'c') compose(); if (event.key === 'e' && selected) archive() }; addEventListener('keydown', handler); return () => removeEventListener('keydown', handler) }, [enabled, messages, selected, open, compose, archive])
}
