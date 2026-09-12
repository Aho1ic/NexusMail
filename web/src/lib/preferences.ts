import { normalizeHexColor } from './accountColors'

export type Preferences = {
  desktopNotifications: boolean
  verificationCodeNotifications: boolean
  autoLoadRemoteImages: boolean
  keyboardShortcuts: boolean
  // account id (as string) -> #rrggbb. Colours are identity chrome, not session
  // state, so they ride with the other preferences in localStorage rather than
  // earning a server round-trip and a contract field.
  accountColors: Record<string, string>
}

const storageKey = 'nexusmail.preferences'

// Defaults preserve the behaviour the app had before settings existed: notify on
// arrival, block remote images until asked, and keep the single-key shortcuts.
export const defaultPreferences: Preferences = {
  desktopNotifications: true,
  verificationCodeNotifications: true,
  autoLoadRemoteImages: false,
  keyboardShortcuts: true,
  accountColors: {},
}

function coerce(value: unknown, fallback: boolean) {
  return typeof value === 'boolean' ? value : fallback
}

// Stored colour maps come from free-typed hex and older shapes, so every entry is
// re-validated; anything that is not a colour is dropped and the account falls
// back to its palette slot.
function coerceColors(value: unknown): Record<string, string> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
  const out: Record<string, string> = {}
  for (const [key, raw] of Object.entries(value as Record<string, unknown>)) {
    const hex = typeof raw === 'string' ? normalizeHexColor(raw) : null
    if (hex) out[key] = hex
  }
  return out
}

// localStorage throws in private browsing modes and when storage is blocked, and
// stored values can be stale from an older shape, so every field falls back to
// its default rather than letting the app render with undefined settings.
export function loadPreferences(): Preferences {
  try {
    const raw = localStorage.getItem(storageKey)
    if (!raw) return defaultPreferences
    const parsed = JSON.parse(raw) as Record<string, unknown>
    return {
      desktopNotifications: coerce(parsed?.desktopNotifications, defaultPreferences.desktopNotifications),
      verificationCodeNotifications: coerce(parsed?.verificationCodeNotifications, defaultPreferences.verificationCodeNotifications),
      autoLoadRemoteImages: coerce(parsed?.autoLoadRemoteImages, defaultPreferences.autoLoadRemoteImages),
      keyboardShortcuts: coerce(parsed?.keyboardShortcuts, defaultPreferences.keyboardShortcuts),
      accountColors: coerceColors(parsed?.accountColors),
    }
  } catch { return defaultPreferences }
}

export function savePreferences(value: Preferences) {
  try { localStorage.setItem(storageKey, JSON.stringify(value)) } catch { /* preferences are best-effort */ }
}

export function notificationPermission(): NotificationPermission | 'unsupported' {
  try { return 'Notification' in window ? Notification.permission : 'unsupported' }
  catch { return 'unsupported' }
}

export async function requestNotificationPermission(): Promise<NotificationPermission | 'unsupported'> {
  try {
    if (!('Notification' in window)) return 'unsupported'
    return await Notification.requestPermission()
  } catch { return 'unsupported' }
}
