import { languageTag } from './i18n'

const formatters = new Map<string, Intl.DateTimeFormat>()
const relativeFormatters = new Map<string, Intl.RelativeTimeFormat>()
const minute = 60_000
const hour = 60 * minute
const day = 24 * hour

export function formatTime(timestamp: string) {
  const tag = languageTag()
  let formatter = formatters.get(tag)
  if (!formatter) {
    formatter = new Intl.DateTimeFormat(tag, { dateStyle: 'medium', timeStyle: 'short' })
    formatters.set(tag, formatter)
  }
  return formatter.format(new Date(timestamp))
}

export function formatRelativeTime(timestamp: string, now: number, { maxDays = Infinity }: { maxDays?: number } = {}) {
  const age = now - Date.parse(timestamp)
  if (age <= -minute || age >= maxDays * day) return formatTime(timestamp)

  const tag = languageTag()
  let formatter = relativeFormatters.get(tag)
  if (!formatter) {
    formatter = new Intl.RelativeTimeFormat(tag, { numeric: 'auto' })
    relativeFormatters.set(tag, formatter)
  }
  if (age >= day) return formatter.format(-Math.floor(age / day), 'day')
  if (age >= hour) return formatter.format(-Math.floor(age / hour), 'hour')
  if (age >= minute) return formatter.format(-Math.floor(age / minute), 'minute')
  return formatter.format(0, 'second')
}
