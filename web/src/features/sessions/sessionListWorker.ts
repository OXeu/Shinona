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
let loading: { controller: AbortController; result: Promise<void> } | undefined
const pending = new Set<AbortController>()

async function check(scope: SessionListScope, signal: AbortSignal) {
  const requestedGeneration = generation
  try {
    const next = await listSessions(scope.project, scope.owner, snapshot, signal)
    signal.throwIfAborted()
    snapshot = next
    checkedGeneration = requestedGeneration
    lastCheck = performance.now()
  } catch (error) {
    signal.throwIfAborted()
    if (error instanceof HttpError && error.status === 401) snapshot = undefined
    throw error
  }
}

function sharedCheck(scope: SessionListScope) {
  if (loading) return loading.result
  const controller = new AbortController()
  const result = check(scope, controller.signal).finally(() => {
    if (loading?.controller === controller) loading = undefined
  })
  loading = { controller, result }
  return result
}

function release(request: AbortController) {
  if (!pending.delete(request) || pending.size > 0) return
  // The next refresh can start immediately, even if the canceled fetch has
  // yet to settle.
  const abandoned = loading
  loading = undefined
  abandoned?.controller.abort()
}

async function refresh(requested: SessionListScope, signal: AbortSignal): Promise<SessionListSnapshot> {
  scope ??= requested
  if (scope.owner !== requested.owner || projectScopeKey(scope.project) !== projectScopeKey(requested.project)) {
    throw new HttpError(400, 'Session list scope does not match the shared worker.')
  }
  if (snapshot && checkedGeneration === generation && performance.now() - lastCheck < freshness) return snapshot
  await sharedCheck(scope)
  signal.throwIfAborted()
  if (checkedGeneration !== generation) {
    // Share one follow-up request if a mutation overlapped the fetch. If it
    // happens again, the next scheduled poll revalidates the latest result.
    await sharedCheck(scope)
    signal.throwIfAborted()
  }
  if (!snapshot) throw new Error('The session list is missing.')
  return snapshot
}

type RefreshRequest = Extract<SessionListWorkerRequest, { type: 'refresh' }>

async function respond(port: MessagePort, message: RefreshRequest, signal: AbortSignal) {
  const { id } = message
  let reply: SessionListWorkerReply
  try {
    const data = await refresh(message.scope, signal)
    reply = message.etag && data.etag === message.etag
      ? { type: 'unchanged', id }
      : { type: 'snapshot', id, snapshot: data }
  } catch (error) {
    const response = error instanceof HttpError ? { status: error.status, code: error.code } : {}
    reply = { type: 'error', id, message: error instanceof Error ? error.message : 'Session list request failed.', ...response }
  }
  if (!signal.aborted) port.postMessage(reply)
}

globalThis.addEventListener('connect', (event) => {
  const [port] = (event as MessageEvent).ports
  if (!port) return
  const requests = new Map<number, AbortController>()
  function cancel(id: number) {
    const request = requests.get(id)
    if (!request) return
    requests.delete(id)
    request.abort()
    release(request)
  }
  port.addEventListener('close', () => {
    for (const id of requests.keys()) cancel(id)
  })
  port.onmessage = ({ data }: MessageEvent<SessionListWorkerRequest>) => {
    switch (data.type) {
      case 'invalidate':
        generation++
        return
      case 'cancel':
        cancel(data.id)
        return
    }
    const { id } = data
    if (requests.has(id)) return
    const request = new AbortController()
    requests.set(id, request)
    pending.add(request)
    void respond(port, data, request.signal).finally(() => {
      if (requests.get(id) === request) requests.delete(id)
      release(request)
    })
  }
})
