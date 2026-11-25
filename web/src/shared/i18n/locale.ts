export const languageTags = {
  en_US: 'en-US',
  zh_CN: 'zh-CN',
  zh_HK: 'zh-HK',
} as const

export type Locale = keyof typeof languageTags

export const defaultLocale: Locale = 'en_US'

export const localeOptions: { value: Locale; label: string; lang: string }[] = [
  { value: 'en_US', lang: languageTags.en_US, label: 'English (US)' },
  { value: 'zh_CN', lang: languageTags.zh_CN, label: '简体中文（中国）' },
  { value: 'zh_HK', lang: languageTags.zh_HK, label: '繁體中文（香港）' },
]

export function isLocale(value: string | null): value is Locale {
  return value !== null && Object.hasOwn(languageTags, value)
}

export function preferredLocale(languages: readonly string[]): Locale {
  for (const language of languages) {
    let tag: Intl.Locale
    try {
      tag = new Intl.Locale(language.replaceAll('_', '-'))
    } catch {
      continue
    }
    if (tag.language === 'en') return 'en_US'
    if (tag.language !== 'zh' && tag.language !== 'yue') continue
    if (tag.script === 'Hans') return 'zh_CN'
    if (tag.script === 'Hant' || tag.language === 'yue' || ['HK', 'MO', 'TW'].includes(tag.region ?? '')) return 'zh_HK'
    return 'zh_CN'
  }
  return defaultLocale
}

export function interpolate(message: string, values: Record<string, string | number>): string {
  return message.replace(/\{(\w+)\}/gu, (token, name: string) => String(values[name] ?? token))
}
