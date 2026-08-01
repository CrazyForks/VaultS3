import { createContext, useContext, useState, useCallback, useEffect, useMemo, type ReactNode } from 'react'
import en from './locales/en.json'
import fr from './locales/fr.json'
import de from './locales/de.json'
import zhCN from './locales/zh-CN.json'

// English is the source of truth: every other locale is checked against it (see
// i18n.test.ts) and any key a translation is missing falls back to the English
// text, so a partial or out-of-date translation degrades to English instead of
// showing a raw key.
export type Messages = Record<string, string>

export interface LocaleDef {
  code: string
  /** Name in the language itself, so it is readable in the switcher regardless of the current locale. */
  label: string
  messages: Messages
}

// The JSON imports are widened to Messages: TypeScript otherwise infers a literal
// key union from en.json, which cannot be indexed by an arbitrary key string.
export const LOCALES: LocaleDef[] = [
  { code: 'en', label: 'English', messages: en as Messages },
  { code: 'de', label: 'Deutsch', messages: de as Messages },
  { code: 'fr', label: 'Français', messages: fr as Messages },
  { code: 'zh-CN', label: '简体中文', messages: zhCN as Messages },
]

const STORAGE_KEY = 'vaults3_locale'

/**
 * Resolve a browser language tag to a locale we ship. Matches the full tag first
 * ("zh-CN"), then the bare language ("de-AT" -> "de"), so a regional variant we
 * do not translate separately still lands on the right language.
 */
export function resolveLocale(tag: string | undefined): string | undefined {
  if (!tag) return undefined
  const lower = tag.toLowerCase()
  const exact = LOCALES.find((l) => l.code.toLowerCase() === lower)
  if (exact) return exact.code
  const base = lower.split('-')[0]
  return LOCALES.find((l) => l.code.toLowerCase().split('-')[0] === base)?.code
}

function getInitialLocale(): string {
  const stored = localStorage.getItem(STORAGE_KEY)
  const fromStorage = resolveLocale(stored ?? undefined)
  if (fromStorage) return fromStorage
  for (const tag of navigator.languages ?? [navigator.language]) {
    const match = resolveLocale(tag)
    if (match) return match
  }
  return 'en'
}

/** Substitute {name} placeholders. An unknown placeholder is left as written. */
export function interpolate(template: string, vars?: Record<string, string | number>): string {
  if (!vars) return template
  return template.replace(/\{(\w+)\}/g, (whole, key: string) =>
    key in vars ? String(vars[key]) : whole,
  )
}

export type TranslateFn = (key: string, vars?: Record<string, string | number>) => string

/**
 * Build a translate function for a locale. Exported so tests (and any non-React
 * caller) can exercise lookup and fallback without mounting a provider.
 */
export function makeTranslator(locale: string): TranslateFn {
  const messages = LOCALES.find((l) => l.code === locale)?.messages ?? (en as Messages)
  return (key, vars) => {
    // en[key] second: an untranslated key shows English, not the key itself.
    const template = messages[key] ?? (en as Messages)[key] ?? key
    return interpolate(template, vars)
  }
}

interface I18nContextValue {
  locale: string
  setLocale: (code: string) => void
  t: TranslateFn
}

const I18nContext = createContext<I18nContextValue>({
  locale: 'en',
  setLocale: () => {},
  t: makeTranslator('en'),
})

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<string>(getInitialLocale)

  // Keep <html lang> in step with the UI language, including the initial render:
  // screen readers pick pronunciation from it, and browsers use it to decide
  // whether to offer a translation of a page that is already translated.
  useEffect(() => {
    document.documentElement.lang = locale
  }, [locale])

  const setLocale = useCallback((code: string) => {
    const resolved = resolveLocale(code) ?? 'en'
    localStorage.setItem(STORAGE_KEY, resolved)
    setLocaleState(resolved)
  }, [])

  const value = useMemo<I18nContextValue>(
    () => ({ locale, setLocale, t: makeTranslator(locale) }),
    [locale, setLocale],
  )

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>
}

export function useI18n() {
  return useContext(I18nContext)
}
