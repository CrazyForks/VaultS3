import { describe, it, expect } from 'vitest'
import React from 'react'
import { renderToString } from 'react-dom/server'
import { LOCALES, makeTranslator, interpolate, resolveLocale, I18nProvider, useI18n, type Messages } from './index'
import en from './locales/en.json'

const english = en as Messages

describe('locale files', () => {
  it('ships English as the source of truth', () => {
    expect(LOCALES[0].code).toBe('en')
    expect(Object.keys(english).length).toBeGreaterThan(0)
  })

  // A translation that invents a key is dead weight: nothing renders it, and it
  // usually means the English key was renamed and the translation not updated.
  it('has no key in a translation that English does not define', () => {
    for (const locale of LOCALES.filter((l) => l.code !== 'en')) {
      const unknown = Object.keys(locale.messages).filter((k) => !(k in english))
      expect({ locale: locale.code, unknown }).toEqual({ locale: locale.code, unknown: [] })
    }
  })

  // Not a hard failure in production (missing keys fall back to English), but a
  // shipped locale should be complete. A community PR adding a new language will
  // see exactly which keys it still owes.
  it('has every English key in every shipped translation', () => {
    for (const locale of LOCALES.filter((l) => l.code !== 'en')) {
      const missing = Object.keys(english).filter((k) => !(k in locale.messages))
      expect({ locale: locale.code, missing }).toEqual({ locale: locale.code, missing: [] })
    }
  })

  // A dropped {placeholder} renders as a sentence with a hole in it.
  it('keeps every placeholder from the English string', () => {
    const names = (s: string) => (s.match(/\{\w+\}/g) ?? []).sort()
    for (const locale of LOCALES.filter((l) => l.code !== 'en')) {
      for (const [key, value] of Object.entries(english)) {
        const translated = locale.messages[key]
        if (translated === undefined) continue
        expect({ locale: locale.code, key, vars: names(translated) }).toEqual({
          locale: locale.code,
          key,
          vars: names(value),
        })
      }
    }
  })

  it('has no empty translation', () => {
    for (const locale of LOCALES) {
      for (const [key, value] of Object.entries(locale.messages)) {
        expect(`${locale.code}:${key}=${value.trim()}`).not.toMatch(/=$/)
      }
    }
  })
})

describe('makeTranslator', () => {
  it('returns the translation for the active locale', () => {
    const t = makeTranslator('de')
    expect(t('common.cancel')).toBe('Abbrechen')
  })

  it('falls back to English rather than showing a raw key', () => {
    // Simulate a translation that has not caught up with a new English key.
    const de = LOCALES.find((l) => l.code === 'de')!
    const saved = de.messages['common.cancel']
    delete de.messages['common.cancel']
    try {
      expect(makeTranslator('de')('common.cancel')).toBe(english['common.cancel'])
    } finally {
      de.messages['common.cancel'] = saved
    }
  })

  it('shows the key itself only when no locale defines it', () => {
    expect(makeTranslator('en')('nope.not.a.key')).toBe('nope.not.a.key')
  })

  it('falls back to English for an unknown locale', () => {
    expect(makeTranslator('xx')('common.cancel')).toBe(english['common.cancel'])
  })

  it('substitutes placeholders', () => {
    expect(makeTranslator('en')('buckets.created', { name: 'app-data' })).toBe(
      'Bucket "app-data" created',
    )
  })
})

describe('interpolate', () => {
  it('leaves an unknown placeholder as written', () => {
    expect(interpolate('a {x} b {y}', { x: '1' })).toBe('a 1 b {y}')
  })

  it('accepts numbers', () => {
    expect(interpolate('{n} objects', { n: 3 })).toBe('3 objects')
  })

  it('is a no-op with no vars', () => {
    expect(interpolate('plain text')).toBe('plain text')
  })
})

describe('resolveLocale', () => {
  it('matches an exact tag', () => {
    expect(resolveLocale('zh-CN')).toBe('zh-CN')
  })

  it('is case-insensitive', () => {
    expect(resolveLocale('ZH-cn')).toBe('zh-CN')
  })

  // A browser set to de-AT should get German, not English.
  it('falls back from a regional variant to the base language', () => {
    expect(resolveLocale('de-AT')).toBe('de')
    expect(resolveLocale('fr-CA')).toBe('fr')
  })

  it('returns undefined for a language we do not ship', () => {
    expect(resolveLocale('ja')).toBeUndefined()
    expect(resolveLocale(undefined)).toBeUndefined()
  })
})

// The provider is what the app actually consumes, so exercise it end to end
// rather than only the translator it wraps. Rendered to a string with
// react-dom/server (already a dependency) so no test-DOM library is needed.
describe('I18nProvider', () => {
  function renderWith(stored: string | null, languages: string[]) {
    // navigator is a getter-only global in Node, so define it rather than assign.
    const def = (name: string, value: unknown) =>
      Object.defineProperty(globalThis, name, { value, configurable: true, writable: true })
    def('localStorage', { getItem: () => stored, setItem: () => {} })
    def('navigator', { languages, language: languages[0] })
    def('document', { documentElement: {} })
    function Probe() {
      const { locale, t } = useI18n()
      return React.createElement('p', null, `${locale}|${t('common.cancel')}`)
    }
    return renderToString(
      React.createElement(I18nProvider, null, React.createElement(Probe, null)),
    )
  }

  it('uses the stored locale when there is one', () => {
    expect(renderWith('fr', ['en-US'])).toContain('fr|Annuler')
  })

  it('detects the browser language when nothing is stored', () => {
    expect(renderWith(null, ['de-DE', 'en'])).toContain('de|Abbrechen')
  })

  it('walks the browser language list past ones we do not ship', () => {
    expect(renderWith(null, ['ja-JP', 'zh-CN'])).toContain('zh-CN|取消')
  })

  it('falls back to English when nothing matches', () => {
    expect(renderWith(null, ['ja-JP'])).toContain('en|Cancel')
  })

  it('ignores a stored locale that is no longer shipped', () => {
    expect(renderWith('kl', ['ja'])).toContain('en|Cancel')
  })
})
