import { senderEmail, senderLabel } from './sender'
import type { Message } from '../types'

export type ListEntry =
  | { kind: 'message'; message: Message }
  | {
      kind: 'stack'
      key: string
      label: string
      email: string
      messages: Message[]
      unread: number
      latest: Message
    }

export type StackMode = 'off' | 'sender' | 'consecutive'

// Consecutive wins when both switches are on: it is the stricter rule and is
// what the advanced setting promises for All Inboxes.
export function resolveStackMode(bySender: boolean, consecutive: boolean): StackMode {
  if (consecutive) return 'consecutive'
  if (bySender) return 'sender'
  return 'off'
}

// Incoming mail only: sent rows are a local copy of outbound traffic and
// stacking them with the inbox would mix two different conversations.
function stackable(message: Message) {
  return message.direction === 'incoming'
}

export function buildListEntries(messages: Message[], mode: StackMode): ListEntry[] {
  if (mode === 'off' || messages.length === 0) {
    return messages.map(message => ({ kind: 'message', message }))
  }
  if (mode === 'consecutive') return consecutiveEntries(messages)
  return senderEntries(messages)
}

function consecutiveEntries(messages: Message[]): ListEntry[] {
  const entries: ListEntry[] = []
  let run: Message[] = []
  const flush = () => {
    if (run.length === 0) return
    if (run.length === 1) entries.push({ kind: 'message', message: run[0] })
    else entries.push(stackFrom(run))
    run = []
  }
  for (const message of messages) {
    if (!stackable(message)) {
      flush()
      entries.push({ kind: 'message', message })
      continue
    }
    const key = senderEmail(message)
    if (run.length > 0 && senderEmail(run[0]) === key) run.push(message)
    else {
      flush()
      run = [message]
    }
  }
  flush()
  return entries
}

function senderEntries(messages: Message[]): ListEntry[] {
  const groups = new Map<string, Message[]>()
  const order: string[] = []
  for (const message of messages) {
    if (!stackable(message)) continue
    const key = senderEmail(message)
    const bucket = groups.get(key)
    if (bucket) bucket.push(message)
    else {
      groups.set(key, [message])
      order.push(key)
    }
  }
  const stacked = new Set(order.filter(key => (groups.get(key)?.length ?? 0) >= 2))
  return messages.map(message => {
    if (!stackable(message)) return { kind: 'message', message }
    const key = senderEmail(message)
    if (!stacked.has(key)) return { kind: 'message', message }
    // One stack row per sender: keep the newest occurrence, skip the rest.
    const bucket = groups.get(key)!
    if (message.id !== bucket[0].id) return null
    return stackFrom(bucket)
  }).filter((entry): entry is ListEntry => entry !== null)
}

function stackFrom(messages: Message[]): ListEntry {
  const latest = messages.reduce((a, b) => (b.received_at >= a.received_at ? b : a))
  return {
    kind: 'stack',
    key: senderEmail(latest),
    label: senderLabel(latest),
    email: senderEmail(latest),
    messages: [...messages].sort((a, b) => b.received_at - a.received_at),
    unread: messages.filter(item => !item.is_read).length,
    latest,
  }
}
