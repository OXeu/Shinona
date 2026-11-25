import { t } from '../../shared/i18n'
import type { MessageKey } from '../../shared/i18n'
import type { RunStatus, SessionSummary } from './sessionApi'

export type SessionAction = 'pin' | 'rename' | 'remove'

const statusKeys: Record<RunStatus, MessageKey> = {
  QUEUED: 'run.queued',
  RUNNING: 'run.running',
  SUCCEEDED: 'run.succeeded',
  FAILED: 'run.failed',
  STOPPED: 'composer.stopped',
}

export function statusLabel(status: RunStatus) {
  return t(statusKeys[status])
}

export function sessionTitle(session: SessionSummary) {
  return session.defaultTitle ? t('sessions.new') : session.title
}

const compactTokens = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 3 })

export function formatCompactTokens(tokens: number) {
  return compactTokens.format(tokens)
}
