import { requestConditional } from '../../shared/http'
import type { Project, SessionSummary } from './sessionApi'

interface SessionList {
  projects: Project[]
  sessions: SessionSummary[]
}

interface ListPatch<T> {
  upsert?: T[]
  order?: string[]
}

interface SessionListPatch {
  delta: {
    projects?: ListPatch<Project>
    sessions?: ListPatch<SessionSummary>
  }
}

export interface SessionListSnapshot extends SessionList {
  etag?: string
}

function applyPatch<T extends { id: string }>(base: T[], patch?: ListPatch<T>): T[] {
  if (!patch) return base
  const entries = new Map(base.map((item) => [item.id, item]))
  for (const item of patch.upsert ?? []) entries.set(item.id, item)
  if (!patch.order) return [...entries.values()]
  return patch.order.map((id) => {
    const item = entries.get(id)
    if (!item) throw new Error('Incomplete session list patch.')
    return item
  })
}

export async function listSessions(projectId: string | null, base: SessionListSnapshot | undefined, signal: AbortSignal): Promise<SessionListSnapshot> {
  const query = new URLSearchParams({ delta: 'true' })
  if (projectId !== null) query.set('projectId', projectId)
  const response = await requestConditional('/api/v1/sessions?' + query, base?.etag, signal)
  if (response.status === 304 && base) return base
  const etag = response.headers.get('ETag') ?? undefined
  const data = await response.json() as SessionList | SessionListPatch
  if (!('delta' in data)) return { ...data, etag }
  if (!base) throw new Error('Missing session list snapshot.')
  return {
    projects: applyPatch(base.projects, data.delta.projects),
    sessions: applyPatch(base.sessions, data.delta.sessions),
    etag,
  }
}
