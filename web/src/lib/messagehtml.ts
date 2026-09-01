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

export function prepareMessageHTML(input: string, attachments: Attachment[], messageID: number, loadRemote: boolean) {
  const document = new DOMParser().parseFromString(input, 'text/html')
  const contentIDs = new Map(attachments.filter(item => item.content_id).map(item => [item.content_id!.replace(/[<>]/g, ''), item.id]))
  document.querySelectorAll<HTMLImageElement>('img[src^="cid:"]').forEach(element => {
    const contentID = element.getAttribute('src')?.slice(4).replace(/[<>]/g, '')
    const attachmentID = contentID ? contentIDs.get(contentID) : undefined
    if (attachmentID) element.src = `/api/v1/messages/${messageID}/attachments/${attachmentID}`
  })
  document.querySelectorAll<HTMLElement>('[data-nexusmail-remote-src]').forEach(element => {
    const source = element.dataset.nexusmailRemoteSrc
    if (loadRemote && source) { element.setAttribute('src', source); return }
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
