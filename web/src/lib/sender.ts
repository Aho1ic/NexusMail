import { decodeAddressList, decodeEncodedWords, displaySender } from './format'
import type { Message } from '../types'

// Grouping must key on the mailbox address, not the display name: the same
// person can send as "张三" one day and "Zhang San <a@b.com>" the next, and a
// name-keyed stack would split in half mid-conversation.
export function senderEmail(message: Message): string {
  const list = decodeAddressList(message.from)
  for (const entry of list) {
    const email = extractEmail(entry)
    if (email) return email.toLowerCase()
  }
  const fromSender = extractEmail(decodeEncodedWords(message.sender))
  if (fromSender) return fromSender.toLowerCase()
  return decodeEncodedWords(message.sender).trim().toLowerCase()
}

export function senderLabel(message: Message): string {
  return displaySender(message.sender) || senderEmail(message)
}

function extractEmail(value: string): string | null {
  const angled = /<([^>]+)>/.exec(value)
  if (angled?.[1]?.includes('@')) return angled[1].trim()
  const bare = value.trim()
  if (/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(bare)) return bare
  return null
}
