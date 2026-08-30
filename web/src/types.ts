export type Account = {
  id: number; email: string; display_name: string; provider: string; status: string;
  last_error?: string; last_connected_at?: number;
}
export type Mailbox = {
  id: number; account_id: number; remote_name: string; display_name: string;
  role: string; sync_mode: string; last_sync_at?: number;
}
export type Message = {
  id: number; account_id: number; direction: 'incoming' | 'outgoing'; subject: string;
  sender: string; recipients: string; from: string; to: string; cc: string; bcc: string;
  snippet: string; body_text?: string; body_html?: string; body_state: string;
  received_at: number; sent_at?: number; is_read: boolean; is_starred: boolean;
  has_attachments: boolean;
}
// unread_total counts the whole view, not the loaded page, so the badge and the
// mark-all-read button stay honest when the view holds more unread mail than one
// page carries.
export type MessagePage = { items: Message[]; next_cursor?: string; unread_total: number }
export type Attachment = { id: number; message_id: number; filename: string; content_type: string; content_id?: string; size_bytes: number; fetch_state: string }
// otp_code is derived by the server on read, so it only ever appears on the detail
// payload and never on a feed row.
export type MessageDetails = { message: Message; attachments: Attachment[]; otp_code?: string }
// capped means more unread mail remained than one call may touch; partial means
// some accounts failed while the reported messages were still applied.
export type MarkReadResult = { updated: number; capped?: boolean; partial?: boolean }
export type Draft = {
  id: number; account_id: number; revision: number; to: string; cc: string; bcc: string;
  subject: string; body_text: string; status: string; remote_sync_state: string; updated_at: number;
  attempt_count?: number; last_error?: string; conflict_of_id?: number;
}
export type DraftInput = { account_id: number; to: string[]; cc: string[]; bcc: string[]; subject: string; body_text: string }
export type EventEnvelope = { type: string; sequence: number; occurred_at: number; data: Record<string, unknown> }
