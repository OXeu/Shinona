import type { ProjectScope } from './projectScope'
import type { SessionListSnapshot } from './sessionListApi'

export const sessionPollInterval = 2_500
export const sessionRequestTimeout = 15_000

export interface SessionListScope {
  owner: string
  project: ProjectScope
}

export type SessionListWorkerRequest =
  | { type: 'refresh'; id: number; scope: SessionListScope; etag?: string }
  | { type: 'invalidate' }

export type SessionListWorkerReply =
  | { type: 'snapshot'; id: number; snapshot: SessionListSnapshot }
  | { type: 'unchanged'; id: number }
  | { type: 'error'; id: number; message: string; status?: number; code?: string }
