import { HttpError } from '../../shared/http'
import { projectScopeKey } from './projectScope'
import { listSessions } from './sessionListApi'
import type { SessionListSnapshot } from './sessionListApi'
import { sessionPollInterval } from './sessionListWorkerMessages'
import type { SessionListScope, SessionListWorkerReply, SessionListWorkerRequest } from './sessionListWorkerMessages'

const freshness = sessionPollInterval - 150
let scope: SessionListScope | undefined
let snapshot: SessionListSnapshot | undefined
let lastCheck = -Infinity
let generation = 0
let checkedGeneration = -1
let loading: Promise<void> | undefined

async function check(scope: SessionListScope) {
  const requestedGeneration = generation
  try {
    snapshot = await listSessions(scope.project, scope.owner, snapshot)
    checkedGeneration = requestedGeneration
    lastCheck = performance.now()
  } catch (error) {
    if (error instanceof HttpError && error.status === 401) snapshot = undefined
    throw error
  } finally {
    loading = undefined
  }
}

async function refresh(requested: SessionListScope): Promise<SessionListSnapshot> {
  scope ??= requested
  if (scope.owner !== requested.owner || projectScopeKey(scope.project) !== projectScopeKey(requested.project)) {
    throw new HttpError(400, 'Session list scope does not match the shared worker.')
  }
  if (snapshot && checkedGeneration === generation && performance.now() - lastCheck < freshness) return snapshot
  loading ??= check(scope)
  await loading
  if (checkedGeneration !== generation) {
    // Share one follow-up request if a mutation overlapped the fetch. If it
    // happens again, the next scheduled poll revalidates the latest result.
    loading ??= check(scope)
    await loading
  }
  if (!snapshot) throw new Error('The session list is missing.')
  return snapshot
}

async function respond(port: MessagePort, message: SessionListWorkerRequest) {
  if (message.type === 'invalidate') {
    generation++
    return
  }
  const { id } = message
  let reply: SessionListWorkerReply
  try {
    const data = await refresh(message.scope)
    reply = message.etag && data.etag === message.etag
      ? { type: 'unchanged', id }
      : { type: 'snapshot', id, snapshot: data }
  } catch (error) {
    const response = error instanceof HttpError ? { status: error.status, code: error.code } : {}
    reply = { type: 'error', id, message: error instanceof Error ? error.message : 'Session list request failed.', ...response }
  }
  port.postMessage(reply)
}

globalThis.addEventListener('connect', (event) => {
  const [port] = (event as MessageEvent).ports
  if (port) port.onmessage = (message: MessageEvent<SessionListWorkerRequest>) => {
    void respond(port, message.data)
  }
})
