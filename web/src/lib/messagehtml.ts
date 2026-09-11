import type { Attachment } from '../types'

// bodyStyles keeps mail readable without overriding the sender's own layout.
// overflow-wrap is break-word rather than anywhere on purpose: anywhere also
// shrinks a cell's min-content width to a single character, which collapsed every
// table-laid-out message into one-glyph-per-line columns that read as mojibake.
// The remaining rules only bound what would otherwise overflow the reading pane.
const bodyStyles = `
  html{-webkit-text-size-adjust:100%}
  body{margin:0;font:15px/1.75 system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;color:#24332d;overflow-wrap:break-word}
  img{max-width:100%;height:auto}
  a{color:#256b50}
  pre{white-space:pre-wrap}
  table{max-width:100%}
  td,th{overflow-wrap:break-word}
  /* Wide fixed-width mail (the 600px newsletter) scrolls in place instead of
     forcing the whole pane sideways or being squeezed out of proportion. */
  .nexusmail-scroll{max-width:100%;overflow-x:auto}
  /* The frame is now much wider than the fixed width most newsletters are built
     for, which would leave a 600px design pinned to the left with the rest of the
     frame empty. Only the outermost table is centred — the wrapper never holds a
     nested one — so no layout inside the sender's design is touched, and a table
     wider than the frame ignores auto margins and still starts at the scroll
     origin. */
  .nexusmail-scroll > table{margin-inline:auto}
  /* Only tables that carry a header row or an explicit border are data tables;
     giving layout tables the same rules would draw grid lines through a design
     that never asked for them. The cell rules key off a class placed on the
     table's own cells rather than a descendant selector, because mail nests
     tables freely and "table.nexusmail-data td" would rule every cell below it. */
  table.nexusmail-data{border-collapse:collapse}
  .nexusmail-cell{border:1px solid #e3e6e1;padding:8px 10px;vertical-align:top}
  th.nexusmail-cell{background:#f4f6f3;text-align:left;font-weight:600}
  /* A blocked image is a placeholder, not content: without a src the browser
     paints its alt text into the flow, and inside the 16-24px boxes mail uses for
     icons that text wraps one glyph per line. Hiding it costs nothing, since the
     alt stays in the DOM for assistive tech and the opt-in button restores the
     real image. */
  img[data-nexusmail-blocked]{visibility:hidden}
`

// The base target sends every link to a new tab. Without it a click navigates the
// frame itself, and because the frame is a sandboxed opaque origin nearly every
// destination refuses to load there — the error the user sees instead of the page.
// allow-popups permits the new tab, allow-popups-to-escape-sandbox keeps that tab
// out of the sandbox so the destination behaves normally. Neither grants the frame
// scripting, same-origin access or top-level navigation.
export function messageDocument(body: string) {
  return `<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="no-referrer">`
    + `<base target="_blank"><style>${bodyStyles}</style></head><body>${body}</body></html>`
}

// Inline images cannot be loaded by the frame itself: it is a sandboxed opaque
// origin, so a request it makes carries no session cookie and the attachment
// endpoint — correctly — answers 401. Granting allow-same-origin would fix the
// cookie and destroy the isolation the server-side sanitizer is backed by, so the
// parent document fetches each inline part with its own credentials instead and
// hands the bytes back here already resolved.
export type InlineRef = { contentID: string; url: string }

// maxInlinePartBytes mirrors the server's own inline cap (maxInlineDraftImportBytes)
// and maxInlineTotalBytes bounds the whole message, because the resolved bytes are
// carried as text inside srcDoc: a part costs ~1.37x its size once base64-encoded,
// and without a ceiling one mail could hold tens of megabytes of it in a string.
// Real inline imagery is logos and signature art, far below either bound; anything
// over them falls back to the blocked-image presentation and stays downloadable
// from the attachment list.
const maxInlinePartBytes = 1 << 20
const maxInlineTotalBytes = 4 << 20

// inlineImageRefs lists the inline parts a body actually references, paired with
// the endpoint each one is served from. Only what the HTML asks for is fetched: a
// message may carry attachments no img names, and those are the user's to download.
export function inlineImageRefs(input: string, attachments: Attachment[], messageID: number): InlineRef[] {
  const document = new DOMParser().parseFromString(input, 'text/html')
  // Content ids are stored with the angle brackets the header carries and written
  // into src without them, so both sides are normalised the same way.
  const parts = new Map(attachments.filter(item => item.content_id).map(item => [item.content_id!.replace(/[<>]/g, ''), item]))
  const refs = new Map<string, string>()
  let budget = maxInlineTotalBytes
  document.querySelectorAll<HTMLImageElement>('img[src^="cid:"]').forEach(element => {
    const contentID = element.getAttribute('src')?.slice(4).replace(/[<>]/g, '') ?? ''
    const part = contentID ? parts.get(contentID) : undefined
    if (!part || refs.has(contentID)) return
    if (part.size_bytes > maxInlinePartBytes || part.size_bytes > budget) return
    budget -= part.size_bytes
    // Deduplicated by content id: one signature logo repeated in a long thread is
    // the same part every time, and is worth exactly one request.
    refs.set(contentID, `/api/v1/messages/${messageID}/attachments/${part.id}`)
  })
  return Array.from(refs, ([contentID, url]) => ({ contentID, url }))
}

// prepareMessageHTML stays synchronous and pure. inlineSources holds the parts the
// caller has already resolved; anything missing from it — not yet fetched, or a
// fetch that failed — is presented as a blocked image rather than left with a src
// no browser can load.
export function prepareMessageHTML(input: string, inlineSources: Map<string, string>, loadRemote: boolean) {
  const document = new DOMParser().parseFromString(input, 'text/html')
  document.querySelectorAll<HTMLImageElement>('img[src^="cid:"]').forEach(element => {
    const contentID = element.getAttribute('src')?.slice(4).replace(/[<>]/g, '') ?? ''
    // Resolved parts arrive as data: URLs. Not blob: — Chrome partitions object
    // URLs by storage key, so this opaque-origin frame cannot resolve one the
    // parent created (measured: it paints nothing even with blob: allowed by CSP,
    // and paints the moment allow-same-origin is added). Do not "optimise" this.
    const resolved = contentID ? inlineSources.get(contentID) : undefined
    if (resolved) { element.setAttribute('src', resolved); return }
    // An unresolved cid: is the same presentation problem as a blocked remote
    // image: keeping the src paints a broken-image glyph, and inside the 16-24px
    // boxes mail uses for icons its alt text wraps one glyph per line. Strip the
    // src and mark it so the stylesheet suppresses both.
    element.removeAttribute('src')
    element.setAttribute('data-nexusmail-blocked', '')
  })
  document.querySelectorAll<HTMLElement>('[data-nexusmail-remote-src]').forEach(element => {
    const source = element.dataset.nexusmailRemoteSrc
    if (loadRemote && source) { element.setAttribute('src', source); element.removeAttribute('data-nexusmail-blocked'); return }
    // Marking the element lets the stylesheet suppress the alt text a src-less
    // image would otherwise paint into the message body.
    if (!element.hasAttribute('src')) element.setAttribute('data-nexusmail-blocked', '')
  })
  annotateTables(document)
  return document.body.innerHTML
}

// annotateTables separates the two jobs mail gives a table. A header row or an
// explicit border means the table holds data and should be ruled; anything else is
// scaffolding for a layout and is left alone. Outermost tables also get a scroll
// wrapper, since a fixed pixel width wider than the pane is the norm in newsletters.
function annotateTables(document: Document) {
  document.querySelectorAll('table').forEach(table => {
    // Both tests must look at the table's own cells only. querySelector('th')
    // searches descendants, so a wrapper holding one data table deep inside was
    // ruled too — in a GitHub notification that marked 7 of 24 tables and drew a
    // border around every layout spacer, which is the grid of empty boxes that
    // made the message look broken.
    const cells = ownCells(table)
    const border = Number(table.getAttribute('border') ?? '0')
    if (cells.some(cell => cell.tagName === 'TH') || border > 0) {
      table.classList.add('nexusmail-data')
      cells.forEach(cell => cell.classList.add('nexusmail-cell'))
    }
    // closest() starts at the element itself, so the ancestor test has to begin
    // one level up; a nested table must not get its own scroll box.
    if (table.parentElement?.closest('table')) return
    const wrapper = document.createElement('div')
    wrapper.className = 'nexusmail-scroll'
    table.replaceWith(wrapper)
    wrapper.append(table)
  })
}

// ownCells returns the cells this table owns, excluding those belonging to a
// nested table.
function ownCells(table: HTMLTableElement) {
  return Array.from(table.querySelectorAll<HTMLTableCellElement>('th,td'))
    .filter(cell => cell.closest('table') === table)
}
