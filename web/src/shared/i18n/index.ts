import { readonly, ref } from 'vue'

import { defaultLocale, interpolate, isLocale, languageTags, preferredLocale } from './locale'
import type { Locale } from './locale'
import en_US from './locales/en_US.json'
import zh_CN from './locales/zh_CN.json'
import zh_HK from './locales/zh_HK.json'

export { localeOptions } from './locale'

export type MessageKey = keyof typeof en_US

const catalogs: Record<Locale, Record<MessageKey, string>> = { en_US, zh_CN, zh_HK }
const storageKey = 'shinona.locale'
const current = ref<Locale>(defaultLocale)

export const locale = readonly(current)

export function languageTag() {
  return languageTags[current.value]
}

export function t(key: MessageKey, values: Record<string, string | number> = {}): string {
  const message = catalogs[current.value][key]
  return interpolate(message, values)
}

function applyLocale(value: Locale) {
  current.value = value
  document.documentElement.lang = languageTag()
}

export function initializeLocale() {
  let saved: string | null = null
  try {
    saved = window.localStorage.getItem(storageKey)
  } catch {
    // Browsers may block access to storage.
  }
  applyLocale(isLocale(saved) ? saved : preferredLocale(navigator.languages))
}

export function setLocale(value: string) {
  if (!isLocale(value)) return
  applyLocale(value)
  try {
    window.localStorage.setItem(storageKey, value)
  } catch {
    // The selected language still applies to this page.
  }
}
