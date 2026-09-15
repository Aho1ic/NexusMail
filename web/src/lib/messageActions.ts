import { copyText } from './notifications'
import { decodeEncodedWords, displaySender } from './format'
import type { Message, MessageDetails } from '../types'

export function isMacLike() {
  try {
    const data = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData
    if (data?.platform) return /mac/i.test(data.platform)
  } catch { /* userAgentData is optional */ }
  return /Mac|iPhone|iPad|iPod/.test(navigator.platform || '') || /Mac OS X/.test(navigator.userAgent)
}

// Chrome never claims text/calendar from a navigation — it always saves the
// file. Safari does hand it to Calendar.app. Detect Chromium so the toast can
// say what actually happened instead of promising an open that will not come.
function isChromeLike() {
  try {
    const data = (navigator as Navigator & { userAgentData?: { brands?: { brand: string }[] } }).userAgentData
    if (data?.brands?.some(entry => /Chromium|Google Chrome|Microsoft Edge/i.test(entry.brand))) return true
  } catch { /* optional */ }
  return /Chrome\//.test(navigator.userAgent) && !/Edg\//.test(navigator.userAgent) || /Edg\//.test(navigator.userAgent)
}

export async function shareMessage(message: Message): Promise<string> {
  const title = message.subject || '（无主题）'
  const text = `${displaySender(message.sender)}\n${title}\n${message.snippet || ''}`.trim()
  if (typeof navigator !== 'undefined' && typeof navigator.share === 'function') {
    try {
      await navigator.share({ title, text })
      return '已分享'
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return ''
    }
  }
  const copied = await copyText(text)
  return copied ? '已复制邮件摘要，可粘贴分享' : '分享失败：无法写入剪贴板'
}

// Print-to-PDF via a sandboxed popup. window.open with "noopener" always
// returns null (spec), so the feature string must not include it — instead the
// document is written into an about:blank window that never gets app privileges
// for scripts: body HTML is injected only as text/plain-equivalent through
// DOM APIs after parsing, never as executable markup from document.write of raw
// HTML from the message.
//
// Safer path: open a blank window, build the print document with DOM methods,
// and only put sanitised/plain content plus escaped markup into it.
export function exportMessagePDF(message: Message, details: MessageDetails | null) {
  const title = message.subject || '（无主题）'
  const bodyText = details?.message.body_text || message.snippet || ''
  const bodyHTML = details?.message.body_html || ''
  // Do not pass raw body_html into document.write: a sanitizer miss would run
  // with the blank window's origin (and opener is null, so it cannot reach the
  // app). Prefer plain text; if only HTML exists, strip tags for the print body.
  const plain = bodyText || stripHTML(bodyHTML)
  const print = window.open('', '_blank', 'width=900,height=700')
  if (!print) return '导出失败：浏览器拦截了打印窗口'
  print.opener = null
  print.document.title = title
  print.document.write(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>${escapeHTML(title)}</title>
<style>
  @page { margin: 18mm 16mm; }
  body { font: 14px/1.7 system-ui, -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; color: #1a1a1a; margin: 0; padding: 24px; }
  header { border-bottom: 1px solid #ddd; padding-bottom: 12px; margin-bottom: 18px; }
  h1 { font-size: 22px; margin: 0 0 8px; }
  .meta { color: #666; font-size: 12px; }
  .body { max-width: 720px; white-space: pre-wrap; }
</style></head><body>
<header><h1>${escapeHTML(title)}</h1>
<div class="meta">${escapeHTML(displaySender(message.sender))} · ${escapeHTML(new Date(message.received_at).toLocaleString('zh-CN'))}</div></header>
<div class="body">${escapeHTML(plain)}</div>
<script>window.onload=function(){window.print()}</script>
</body></html>`)
  print.document.close()
  return '已打开打印视图，选择「存储为 PDF」即可'
}

function stripHTML(input: string) {
  if (!input) return ''
  try {
    const doc = new DOMParser().parseFromString(input, 'text/html')
    return doc.body.textContent?.replace(/\n{3,}/g, '\n\n').trim() ?? ''
  } catch {
    return input.replace(/<[^>]+>/g, ' ')
  }
}

// Heuristic date pull for a one-click ICS. Best-effort: when nothing parses the
// caller reports that instead of writing a calendar file with a fake date.
export function buildCalendarICS(message: Message, details: MessageDetails | null): { ics: string; when: Date } | null {
  const text = `${message.subject || ''}\n${details?.message.body_text || message.snippet || ''}`
  const when = extractDate(text) ?? extractDate(new Date(message.received_at).toLocaleString('zh-CN'))
  if (!when) return null
  const end = new Date(when.getTime() + 60 * 60 * 1000)
  const stamp = (value: Date) => value.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '')
  const summary = (message.subject || '邮件日程').replace(/\r?\n/g, ' ')
  const description = `${displaySender(message.sender)}\n${(details?.message.body_text || message.snippet || '').slice(0, 500)}`
  const ics = [
    'BEGIN:VCALENDAR',
    'VERSION:2.0',
    'PRODID:-//NexusMail//ZH',
    'BEGIN:VEVENT',
    `UID:nexusmail-${message.id}@local`,
    `DTSTAMP:${stamp(new Date())}`,
    `DTSTART:${stamp(when)}`,
    `DTEND:${stamp(end)}`,
    `SUMMARY:${escapeICS(summary)}`,
    `DESCRIPTION:${escapeICS(description)}`,
    'END:VEVENT',
    'END:VCALENDAR',
  ].join('\r\n')
  return { ics, when }
}

// Open the OS calendar with the event pre-filled.
//
// Path order matters:
// 1. Web Share with a File — Chromium on macOS can put Calendar in the share
//    sheet, which is the only pure-web way to avoid a manual Downloads step.
// 2. Navigate to the inline ICS endpoint — Safari claims text/calendar and
//    opens Calendar.app. Chrome downloads instead (browser policy, not ours).
// 3. Explicit download + clear guidance when everything else is blocked.
export async function openSystemCalendar(message: Message, details: MessageDetails | null): Promise<string> {
  const built = buildCalendarICS(message, details)
  const ics = built?.ics ?? buildFallbackICS(message)
  const file = new File([ics], 'nexusmail-event.ics', { type: 'text/calendar' })

  const nav = navigator as Navigator & { canShare?: (data: ShareData) => boolean }
  if (typeof nav.share === 'function' && nav.canShare?.({ files: [file] })) {
    try {
      await nav.share({ files: [file], title: message.subject || '添加到日历' })
      return '请在系统分享菜单中选择「日历」完成添加'
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return ''
      // NotSupportedError / user denied file share → fall through.
    }
  }

  const endpoint = `/api/v1/messages/${message.id}/calendar`
  const win = window.open(endpoint, '_blank', 'noopener,noreferrer')
  if (win) {
    if (isChromeLike()) {
      return 'Chrome 不会直接唤起日历，已打开 .ics 下载：点下载项打开即可加入；也可在下载菜单选择「始终打开此类文件」'
    }
    return '已打开系统日历，请在弹出的窗口中确认添加日程'
  }

  downloadTextFile(`nexusmail-${message.id}.ics`, ics, 'text/calendar;charset=utf-8')
  return '浏览器拦截了窗口，已改为下载 .ics；打开该文件即可加入系统日历'
}

function buildFallbackICS(message: Message) {
  const when = message.received_at || Date.now()
  const end = when + 60 * 60 * 1000
  const stamp = (ms: number) => new Date(ms).toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '')
  const summary = (message.subject || '邮件日程').replace(/\r?\n/g, ' ')
  return [
    'BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//NexusMail//ZH',
    'BEGIN:VEVENT',
    `UID:nexusmail-${message.id}@local`,
    `DTSTAMP:${stamp(Date.now())}`,
    `DTSTART:${stamp(when)}`,
    `DTEND:${stamp(end)}`,
    `SUMMARY:${escapeICS(summary)}`,
    'END:VEVENT', 'END:VCALENDAR',
  ].join('\r\n')
}

export function downloadTextFile(filename: string, content: string, mime: string) {
  const blob = new Blob([content], { type: mime })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.click()
  URL.revokeObjectURL(url)
}

function extractDate(text: string): Date | null {
  const patterns: RegExp[] = [
    /(\d{4})[-/年.](\d{1,2})[-/月.](\d{1,2})[日号]?\s*(\d{1,2})?:?(\d{2})?/,
    /(\d{1,2})[-/月.](\d{1,2})[日号]?\s*(\d{1,2})?:?(\d{2})?/,
  ]
  for (const pattern of patterns) {
    const match = pattern.exec(text)
    if (!match) continue
    const now = new Date()
    let year = now.getFullYear()
    let month: number
    let day: number
    let hour = 9
    let minute = 0
    if (match[0].includes('年') || Number(match[1]) > 31) {
      year = Number(match[1]); month = Number(match[2]); day = Number(match[3])
      if (match[4]) hour = Number(match[4])
      if (match[5]) minute = Number(match[5])
    } else {
      month = Number(match[1]); day = Number(match[2])
      if (match[3]) hour = Number(match[3])
      if (match[4]) minute = Number(match[4])
    }
    if (!month || !day || month > 12 || day > 31) continue
    const date = new Date(year, month - 1, day, hour, minute)
    if (Number.isNaN(date.getTime())) continue
    return date
  }
  const parsed = Date.parse(text)
  return Number.isNaN(parsed) ? null : new Date(parsed)
}

function escapeHTML(value: string) {
  return value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
}

function escapeICS(value: string) {
  return value.replace(/\\/g, '\\\\').replace(/;/g, '\\;').replace(/,/g, '\\,').replace(/\r?\n/g, '\\n')
}

export function decodeSubject(subject: string) {
  return decodeEncodedWords(subject)
}
