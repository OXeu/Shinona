import { t } from '../../shared/i18n'
import type { SessionSummary } from './sessionApi'

export type SessionAction = 'pin' | 'rename' | 'remove'

export function sessionTitle(session: SessionSummary) {
  return session.defaultTitle ? t('sessions.new') : session.title
}

const compactTokens = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 3 })

export function formatCompactTokens(tokens: number) {
  return compactTokens.format(tokens)
}
