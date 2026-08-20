export type Preferences = {
  desktopNotifications: boolean
  verificationCodeNotifications: boolean
  autoLoadRemoteImages: boolean
  keyboardShortcuts: boolean
}

const storageKey = 'nexusmail.preferences'

// Defaults preserve the behaviour the app had before settings existed: notify on
// arrival, block remote images until asked, and keep the single-key shortcuts.
export const defaultPreferences: Preferences = {
  desktopNotifications: true,
  verificationCodeNotifications: true,
  autoLoadRemoteImages: false,
  keyboardShortcuts: true,
}

function coerce(value: unknown, fallback: boolean) {
  return typeof value === 'boolean' ? value : fallback
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
