// The provider catalogue is the one place that knows how a mail service is named,
// how it authenticates, and which backend provider value it posts as. It exists
// because those three facts had been inlined at every call site: the dialog held
// `['qq','163','gmail','outlook']` and rendered it through `uppercase`, which
// printed GMAIL and OUTLOOK — neither is how the brands are written — and the
// account list did the same to its badge.
//
// `label` is the brand's own spelling and is used verbatim: no CSS transform is
// applied to it anywhere.
//
// `backend` is deliberately not `id`. Hotmail is Outlook: the same Microsoft
// authorization endpoint, the same IMAP host, the same scopes. It is listed
// separately because a user with an @hotmail.com address does not necessarily
// know to pick "Outlook", but it posts as `outlook` — accounts.provider carries an
// enumerated CHECK constraint, and duplicating a preset to win a label would need
// a migration for no behavioural gain.

type ProviderBase = {
  id: string
  label: string
  backend: string
  hint: string
}

// Discriminated on `auth` so the credential label cannot be read on a path that
// has no credential, and cannot be forgotten on one that does. The dialog derives
// `auth.type` from this field too, because posting `oauth2` for a password
// provider — or the reverse — is rejected by the accounts.auth_type CHECK after
// the preset lookup has already decided the truth.
//
// `credential` is what the service itself calls the secret it issues. The Chinese
// providers issue a 客户端授权码; Apple issues an App 专用密码 and calls it nothing
// else, so one hardcoded 授权码 label sent an iCloud user looking through Apple ID
// settings for a thing that does not exist there.
export type ProviderOption =
  | (ProviderBase & { auth: 'password'; credential: string })
  | (ProviderBase & { auth: 'oauth2' })

const CODE = '授权码'

export const providerOptions: ProviderOption[] = [
  { id: 'qq', label: 'QQ', backend: 'qq', auth: 'password', hint: `使用${CODE}登录`, credential: CODE },
  { id: '163', label: '163', backend: '163', auth: 'password', hint: `使用${CODE}登录`, credential: CODE },
  { id: '126', label: '126', backend: '126', auth: 'password', hint: `使用${CODE}登录`, credential: CODE },
  { id: 'gmail', label: 'Gmail', backend: 'gmail', auth: 'oauth2', hint: '使用网页授权登录' },
  { id: 'outlook', label: 'Outlook', backend: 'outlook', auth: 'oauth2', hint: '使用网页授权登录' },
  { id: 'hotmail', label: 'Hotmail', backend: 'outlook', auth: 'oauth2', hint: '使用网页授权登录' },
  { id: 'icloud', label: 'iCloud', backend: 'icloud', auth: 'password', hint: '使用 App 专用密码登录', credential: 'App 专用密码' },
]

// Accounts come back carrying the backend value, so the badge needs the reverse
// lookup. Hotmail accounts read as Outlook here, which is what they are.
export function providerLabel(backend: string): string {
  return providerOptions.find(option => option.backend === backend)?.label ?? backend
}

// The marks are drawn inline rather than fetched: an <img> per provider would be
// one request each for 16px of decoration, and the CSP allows no third-party
// origin. They are simplified brand-coloured glyphs, not exact logotypes — enough
// for a 16px chip to be identifiable at a glance. Decorative: every icon sits next
// to its own text label, so announcing it again would only be noise.
export function ProviderIcon({ id, size = 16 }: { id: string; size?: number }) {
  const props = { width: size, height: size, viewBox: '0 0 16 16', 'aria-hidden': true, focusable: false } as const
  switch (id) {
    case 'qq':
      return <svg {...props}>
        <rect width="16" height="16" rx="4" fill="#12B7F5" />
        <path d="M8 3.2c-1.7 0-3 1.4-3 3.1 0 .5-.2 1-.5 1.5-.4.7-.6 1.2-.6 1.7 0 .5.3.8.8.8.3 0 .6-.2.9-.5.6.3 1.5.5 2.4.5s1.8-.2 2.4-.5c.3.3.6.5.9.5.5 0 .8-.3.8-.8 0-.5-.2-1-.6-1.7-.3-.5-.5-1-.5-1.5 0-1.7-1.3-3.1-3-3.1Z" fill="#fff" />
        <circle cx="6.7" cy="6.4" r=".7" fill="#12B7F5" />
        <circle cx="9.3" cy="6.4" r=".7" fill="#12B7F5" />
      </svg>
    case '163':
      return <svg {...props}>
        <rect width="16" height="16" rx="4" fill="#D93327" />
        <path d="M4.3 5.1h7.4v1.3H8.6v.9h3.1v4.6H4.3V7.3h3v-.9H4.3V5.1Zm1.2 3.4v2.2h5v-2.2h-5Z" fill="#fff" />
        <path d="M4.3 3.4h7.4v1.1H4.3V3.4Z" fill="#fff" opacity=".75" />
      </svg>
    case '126':
      // Same NetEase red as 163, since that is the brand's colour for both, with an
      // envelope rather than a numeral glyph so the two chips do not read as the
      // same mark at 16px.
      return <svg {...props}>
        <rect width="16" height="16" rx="4" fill="#D93327" />
        <rect x="3.4" y="5.2" width="9.2" height="5.6" rx=".8" fill="#fff" />
        <path d="M4.4 6.3 8 8.7l3.6-2.4" stroke="#D93327" strokeWidth="1" strokeLinecap="round" fill="none" />
      </svg>
    case 'gmail':
      return <svg {...props}>
        <rect x=".5" y="3" width="15" height="10" rx="1.8" fill="#fff" stroke="#E3E3E3" strokeWidth=".6" />
        <path d="M.5 4.8v6.4A1.8 1.8 0 0 0 2.3 13h1.4V7.1L.5 4.8Z" fill="#4285F4" />
        <path d="M12.3 13h1.4a1.8 1.8 0 0 0 1.8-1.8V4.8l-3.2 2.3V13Z" fill="#34A853" />
        <path d="M3.7 13V7.1L8 10.2l4.3-3.1V13H3.7Z" fill="#EA4335" />
        <path d="M15.5 4.8V4.4c0-.9-1-1.5-1.8-1L8 7.5 2.3 3.4c-.8-.5-1.8.1-1.8 1v.4L8 10.2l7.5-5.4Z" fill="#FBBC05" />
        <path d="M15.5 4.8 8 10.2.5 4.8v-.4c0-.9 1-1.5 1.8-1L8 7.5l5.7-4.1c.8-.6 1.8 0 1.8 1v.4Z" fill="#C5221F" opacity=".18" />
      </svg>
    case 'outlook':
      return <svg {...props}>
        <path d="M6.6 3h7.6c.4 0 .8.4.8.8v8.4c0 .4-.4.8-.8.8H6.6V3Z" fill="#0F6CBD" />
        <path d="M6.6 4.3h8.4v3.9H6.6V4.3Z" fill="#28A8EA" opacity=".55" />
        <path d="M1 3.7 8 2.4v11.2L1 12.3V3.7Z" fill="#0364B8" />
        <path d="M4.5 5.6c1.2 0 2 1 2 2.4s-.8 2.4-2 2.4-2-1-2-2.4.8-2.4 2-2.4Zm0 1.1c-.6 0-1 .5-1 1.3s.4 1.3 1 1.3 1-.5 1-1.3-.4-1.3-1-1.3Z" fill="#fff" />
      </svg>
    case 'hotmail':
      // Hotmail's own logotype was retired into Outlook's, so the Microsoft
      // four-square is the honest mark here — and it stays distinguishable from
      // the blue Outlook chip beside it.
      return <svg {...props}>
        <rect x="1.4" y="1.4" width="6" height="6" fill="#F25022" />
        <rect x="8.6" y="1.4" width="6" height="6" fill="#7FBA00" />
        <rect x="1.4" y="8.6" width="6" height="6" fill="#00A4EF" />
        <rect x="8.6" y="8.6" width="6" height="6" fill="#FFB900" />
      </svg>
    case 'icloud':
      // A cloud on a white plate: the chip sits on the paper background, and a
      // plain blue cloud on cream reads as a hole rather than a mark.
      return <svg {...props}>
        <rect x=".5" y=".5" width="15" height="15" rx="4" fill="#fff" stroke="#E3E3E3" strokeWidth=".6" />
        <path d="M5.1 11.1a2.1 2.1 0 0 1-.2-4.2 3 3 0 0 1 5.6-.8 2.4 2.4 0 0 1 .4 4.9l-.2.1H5.1Z" fill="#3E9CDC" />
        <path d="M5.1 11.1a2.1 2.1 0 0 1-.2-4.2 3 3 0 0 1 2.5-1.6 3 3 0 0 0-1.1 5.8H5.1Z" fill="#7BC5EE" />
      </svg>
    default:
      return <svg {...props}><rect width="16" height="16" rx="4" fill="#214F3B" opacity=".2" /></svg>
  }
}
