import { HttpError } from '../../shared/http'
import { projectScopeKey } from './projectScope'
import type { ProjectScope } from './projectScope'
import { listSessions } from './sessionListApi'
import SharedSessionList from './sessionListWorker?sharedworker'
import type { SessionListSnapshot } from './sessionListApi'
import { sessionRequestTimeout } from './sessionListWorkerMessages'
import type { SessionListWorkerReply, SessionListWorkerRequest } from './sessionListWorkerMessages'

interface PendingRefresh {
  receive(reply: SessionListWorkerReply): void
  settle(error?: unknown): void
}

export class SessionListPolling {
  private port: MessagePort | undefined
  private pending = new Map<number, PendingRefresh>()
  private nextID = 0

  constructor(private readonly owner: string, private readonly project: ProjectScope) {
    if (typeof SharedWorker === 'undefined') return
    try {
      const worker = new SharedSessionList({
        name: JSON.stringify(['shinona-session-list', owner, projectScopeKey(project)]),
      })
      this.port = worker.port
      this.port.onmessage = (event: MessageEvent<SessionListWorkerReply>) => {
        this.pending.get(event.data.id)?.receive(event.data)
      }
      worker.addEventListener('error', () => this.releaseWorker())
    } catch {
      // Browser policy can disable shared workers.
    }
  }

  changed() {
    this.send({ type: 'invalidate' })
  }

  close() {
    this.releaseWorker(new DOMException('Session list scope closed.', 'AbortError'))
  }

  private releaseWorker(error?: unknown) {
    this.port?.close()
    this.port = undefined
    for (const request of this.pending.values()) request.settle(error)
  }

  async refresh(base: SessionListSnapshot | undefined, signal: AbortSignal): Promise<SessionListSnapshot> {
    signal.throwIfAborted()
    if (this.port) {
      const response = await this.refreshShared(base, signal)
      if (response) return response
    }
    return listSessions(this.project, this.owner, base, signal)
  }

  private send(message: SessionListWorkerRequest) {
    this.port?.postMessage(message)
  }

  private refreshShared(base: SessionListSnapshot | undefined, signal: AbortSignal) {
    const id = ++this.nextID
    return new Promise<SessionListSnapshot | undefined>((resolve, reject) => {
      const finish = () => {
        clearTimeout(timeout)
        signal.removeEventListener('abort', abort)
        this.pending.delete(id)
      }
      const abort = () => {
        finish()
        reject(signal.reason)
      }
      const timeout = setTimeout(() => this.releaseWorker(), 2 * sessionRequestTimeout + 5_000)
      signal.addEventListener('abort', abort, { once: true })
      this.pending.set(id, {
        receive(reply) {
          finish()
          switch (reply.type) {
            case 'snapshot':
              resolve(reply.snapshot)
              break
            case 'unchanged':
              resolve(base)
              break
            case 'error':
              reject(reply.status === undefined ? new Error(reply.message) : new HttpError(reply.status, reply.message, reply.code))
              break
          }
        },
        settle(error) {
          finish()
          if (error === undefined) resolve(undefined)
          else reject(error)
        },
      })
      this.send({ type: 'refresh', id, scope: { owner: this.owner, project: this.project }, etag: base?.etag })
    })
  }
}
