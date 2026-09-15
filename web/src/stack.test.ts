import { describe, expect, it } from 'vitest'
import { buildListEntries, resolveStackMode } from './lib/stack'
import { senderEmail } from './lib/sender'
import type { Message } from './types'

function message(id: number, sender: string, fromEmail: string, receivedAt = id, direction: Message['direction'] = 'incoming'): Message {
  return {
    id,
    account_id: 1,
    direction,
    subject: `Subject ${id}`,
    sender,
    recipients: 'me@example.com',
    from: JSON.stringify([`Name <${fromEmail}>`]),
    to: '[]',
    cc: '[]',
    bcc: '[]',
    snippet: 'preview',
    body_state: 'ready',
    received_at: receivedAt,
    is_read: true,
    is_starred: false,
    has_attachments: false,
  }
}

describe('sender stacking', () => {
  it('prefers consecutive when both switches are on', () => {
    expect(resolveStackMode(true, true)).toBe('consecutive')
    expect(resolveStackMode(true, false)).toBe('sender')
    expect(resolveStackMode(false, false)).toBe('off')
  })

  it('groups non-adjacent mail from one sender in basic mode', () => {
    const messages = [
      message(3, 'A <a@x.com>', 'a@x.com', 3),
      message(2, 'B <b@x.com>', 'b@x.com', 2),
      message(1, 'A <a@x.com>', 'a@x.com', 1),
    ]
    const entries = buildListEntries(messages, 'sender')
    expect(entries).toHaveLength(2)
    const stack = entries.find(entry => entry.kind === 'stack')
    expect(stack).toBeTruthy()
    if (stack?.kind !== 'stack') throw new Error('expected stack')
    expect(stack.messages.map(item => item.id).sort()).toEqual([1, 3])
    expect(stack.email).toBe('a@x.com')
  })

  it('only stacks adjacent runs in consecutive mode', () => {
    const messages = [
      message(3, 'A <a@x.com>', 'a@x.com', 3),
      message(2, 'B <b@x.com>', 'b@x.com', 2),
      message(1, 'A <a@x.com>', 'a@x.com', 1),
    ]
    const entries = buildListEntries(messages, 'consecutive')
    expect(entries.every(entry => entry.kind === 'message')).toBe(true)

    const adjacent = [
      message(2, 'A <a@x.com>', 'a@x.com', 2),
      message(1, 'A <a@x.com>', 'a@x.com', 1),
      message(0, 'B <b@x.com>', 'b@x.com', 0),
    ]
    const stacked = buildListEntries(adjacent, 'consecutive')
    expect(stacked[0].kind).toBe('stack')
    expect(stacked[1].kind).toBe('message')
  })

  it('does not stack outgoing rows', () => {
    const messages = [
      message(2, 'A <a@x.com>', 'a@x.com', 2, 'outgoing'),
      message(1, 'A <a@x.com>', 'a@x.com', 1, 'outgoing'),
    ]
    expect(buildListEntries(messages, 'sender').every(entry => entry.kind === 'message')).toBe(true)
  })

  it('keys on the mailbox address, not the display name', () => {
    expect(senderEmail(message(1, '张三', 'a@x.com'))).toBe('a@x.com')
    expect(senderEmail(message(2, 'Zhang San', 'A@X.com'))).toBe('a@x.com')
  })
})
