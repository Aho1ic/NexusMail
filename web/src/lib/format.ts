export function splitEmails(value: string) { return value.split(/[,;\n]/).map(item => item.trim()).filter(Boolean) }
export function decodeAddressList(value: string) { try { const parsed = JSON.parse(value); return Array.isArray(parsed) ? parsed.map(String) : [] } catch { return [] } }
export function displaySender(value: string) { const decoded = decodeEncodedWords(value); return decoded.replace(/<.*?>/g, '').replace(/["']/g, '').trim() || decoded }

const encodedWord = /=\?([^?]+)\?([bqBQ])\?([^?]*)\?=/g

// decodeEncodedWords renders RFC 2047 encoded-words as text. The server stores the
// decoded form now, but rows synced before that still hold "=?utf-8?q?...?=", and a
// provider can also hand back a header the Go decoder could not parse. Decoding at
// display time covers both without a migration.
export function decodeEncodedWords(value: string) {
  if (!value.includes('=?')) return value
  return value.replace(encodedWord, (whole, charset: string, encoding: string, payload: string) => {
    try {
      // Q encoding writes a space as "_" and everything else as =XX octets.
      const bytes = encoding.toLowerCase() === 'b'
        ? Uint8Array.from(atob(payload.replace(/\s/g, '')), character => character.charCodeAt(0))
        : Uint8Array.from(payload.replace(/_/g, ' ').replace(/=([0-9a-fA-F]{2})/g, (_, hex: string) => String.fromCharCode(parseInt(hex, 16))), character => character.charCodeAt(0))
      return new TextDecoder(charset.split('*')[0]).decode(bytes)
    } catch { return whole }
  })
}
export function formatDate(value: number) { const date = new Date(value); const today = new Date(); return date.toDateString() === today.toDateString() ? date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : date.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' }) }
export function formatFullDate(value: number) { return new Date(value).toLocaleString('zh-CN', { dateStyle: 'long', timeStyle: 'short' }) }
export function formatBytes(value: number) { if (value < 1024) return `${value} B`; if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`; return `${(value / 1024 / 1024).toFixed(1)} MB` }
export function messageOf(error: unknown) { return error instanceof Error ? error.message : '发生未知错误' }
export function accountStatusLabel(status: string) {
  const label: Record<string, string> = { connected: '已连接', connecting: '正在连接', syncing: '正在同步', backoff: '连接异常，正在重试', disconnected: '未连接' }
  return label[status] || status
}
