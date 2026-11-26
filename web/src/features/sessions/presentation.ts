import { computed } from 'vue'

import { languageTag, t } from '../../shared/i18n'
import type { SessionSummary } from './sessionApi'

export type SessionAction = 'pin' | 'rename' | 'remove'

export function sessionTitle(session: SessionSummary) {
  return session.defaultTitle ? t('sessions.new') : session.title
}

const compactTokens = computed(() => new Intl.NumberFormat(languageTag(), { notation: 'compact', maximumFractionDigits: 3 }))

export function formatCompactTokens(tokens: number) {
  return compactTokens.value.format(tokens)
}
