import { describe, expect, it } from 'vitest'
import { accountColor, accountPalette, chipStyle, defaultAccountColor, normalizeHexColor } from './lib/accountColors'

// The chip paints an account's name in "its" colour, so the mapping from account
// to colour has to be stable, overridable, and incapable of producing an unreadable
// or invalid style from whatever a settings field hands it.

describe('normalizeHexColor', () => {
  it('canonicalises the accepted forms and rejects the rest', () => {
    expect(normalizeHexColor('#5E87B0')).toBe('#5e87b0')
    expect(normalizeHexColor('abc')).toBe('#aabbcc')
    expect(normalizeHexColor('  #FFFFFF ')).toBe('#ffffff')
    expect(normalizeHexColor('')).toBeNull()
    expect(normalizeHexColor('#12345')).toBeNull()
    expect(normalizeHexColor('green')).toBeNull()
    // A 6-digit form with alpha is not a colour this app parses; accepting the
    // first 6 characters would silently change what the user typed.
    expect(normalizeHexColor('#5e87b0ff')).toBeNull()
  })
})

describe('defaultAccountColor', () => {
  it('maps an account id to a stable palette slot', () => {
    expect(accountPalette).toContain(defaultAccountColor(1))
    expect(defaultAccountColor(1)).toBe(defaultAccountColor(1))
    // The slot comes from the id alone, so it survives renames and re-sorts.
    expect(defaultAccountColor(2)).not.toBe(defaultAccountColor(1))
  })
})

describe('accountColor', () => {
  it('prefers an explicit override and falls back to the palette slot', () => {
    expect(accountColor(1, { '1': '#be6480' })).toBe('#be6480')
    expect(accountColor(1)).toBe(defaultAccountColor(1))
    // A stale or mistyped override must not reach a style — the palette wins.
    expect(accountColor(1, { '1': 'not-a-colour' })).toBe(defaultAccountColor(1))
    expect(accountColor(1, {})).toBe(defaultAccountColor(1))
  })
})

describe('chipStyle', () => {
  it('tints the background and keeps the text in the account colour', () => {
    const style = chipStyle('#be6480')
    expect(style.backgroundColor).toBe('rgba(190, 100, 128, 0.14)')
    expect(style.color).toMatch(/^#[0-9a-f]{6}$/)
  })

  it('darkens colours too light to read on the paper background', () => {
    const style = chipStyle('#f5f5f5')
    // WCAG contrast of the final text against the white it sits on must clear 3:1;
    // the input colour sits at roughly 1.1:1, unreadable at chip size.
    const hex = style.color!
    const channel = (raw: number) => {
      const c = raw / 255
      return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
    }
    const luminance = 0.2126 * channel(parseInt(hex.slice(1, 3), 16))
      + 0.7152 * channel(parseInt(hex.slice(3, 5), 16))
      + 0.0722 * channel(parseInt(hex.slice(5, 7), 16))
    expect((1.05) / (luminance + 0.05)).toBeGreaterThanOrEqual(3)
  })
})
