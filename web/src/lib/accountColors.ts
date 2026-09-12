// Per-account identity colours. The message-list chip and the settings card both
// read from here, so "the colour of an account" has exactly one definition: an
// explicit override stored with the preferences, or — while the user has not
// picked one — a stable slot in the palette keyed by the account id. The slot is
// derived from the id alone, so it survives re-sorts, renames and reloads.
//
// The palette is deliberately low-saturation: dopamine hues (coral, matcha, corn
// flower, honey, lilac, rose, teal, olive) pulled back to mid-tones so they read
// as accents on the paper background instead of competing with unread bold text.

export const accountPalette = [
  '#c4705a', // 陶土珊瑚
  '#7c9a62', // 抹茶
  '#5e87b0', // 雾蓝
  '#b9903f', // 蜜金
  '#937cb3', // 藕紫
  '#be6480', // 玫瑰
  '#4e938f', // 湖绿
  '#8f9c4a', // 橄榄
] as const

type RGB = { r: number; g: number; b: number }

// Accepts #RGB and #RRGGBB (with or without the hash, case-insensitive) and
// returns the canonical #rrggbb form, or null when the input is not a colour —
// free-typed hex in settings must fail closed rather than reach a style.
export function normalizeHexColor(input: string): string | null {
  const value = input.trim().replace(/^#/, '')
  if (!/^[0-9a-fA-F]{3}$|^[0-9a-fA-F]{6}$/.test(value)) return null
  const full = value.length === 3 ? value.split('').map(ch => ch + ch).join('') : value
  return `#${full.toLowerCase()}`
}

function parseHex(hex: string): RGB {
  const value = normalizeHexColor(hex) ?? '#888888'
  return {
    r: parseInt(value.slice(1, 3), 16),
    g: parseInt(value.slice(3, 5), 16),
    b: parseInt(value.slice(5, 7), 16),
  }
}

function luminance({ r, g, b }: RGB): number {
  // WCAG relative luminance on the 0..1 scale; contrast is computed against the
  // white-ish paper the chips sit on.
  const channel = (raw: number) => {
    const c = raw / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b)
}

// The chip paints its text in the account colour itself. A colour the user typed
// by hand can be near-white, where that text would vanish, so it is pulled toward
// black until it carries at least 3:1 contrast on the light background.
function readableText(hex: string): string {
  let current = parseHex(hex)
  let text = hex
  for (let step = 0; step < 12 && luminance(current) > 0.23; step++) {
    current = { r: current.r * 0.88, g: current.g * 0.88, b: current.b * 0.88 }
    text = `#${[current.r, current.g, current.b].map(c => Math.round(c).toString(16).padStart(2, '0')).join('')}`
  }
  return text
}

// The chip background is a flat tint of the colour at low alpha; over the row's
// white/paper surfaces that stays quiet enough for 10px text.
export function chipStyle(hex: string): React.CSSProperties {
  const { r, g, b } = parseHex(hex)
  return { backgroundColor: `rgba(${r}, ${g}, ${b}, 0.14)`, color: readableText(hex) }
}

export function defaultAccountColor(accountID: number): string {
  return accountPalette[Math.abs(accountID) % accountPalette.length]
}

// overrides maps account id (as string — localStorage keys are strings) to hex.
// An override that no longer parses falls through to the default slot instead of
// poisoning the style.
export function accountColor(accountID: number, overrides?: Record<string, string>): string {
  const custom = overrides?.[String(accountID)]
  const normalized = custom ? normalizeHexColor(custom) : null
  return normalized ?? defaultAccountColor(accountID)
}
