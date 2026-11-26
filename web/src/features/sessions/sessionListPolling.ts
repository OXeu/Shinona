import { listSessions } from './sessionListApi'
import type { SessionListSnapshot } from './sessionListApi'

export const sessionPollInterval = 2_500
const sharedFreshness = sessionPollInterval - 150

interface SharedPoll {
  started: number
  etag?: string
  snapshot?: SessionListSnapshot
}

type PollMessage = SharedPoll | { request: true }

export class SessionListPolling {
  private channel: BroadcastChannel | undefined
  private shared: SharedPoll | undefined
  private changedAt = 0
  private readonly name: string

  constructor(owner: string | undefined, private readonly projectId: string | null) {
    this.name = JSON.stringify(['shinona-session-list', owner, projectId])
    if (owner && typeof BroadcastChannel !== 'undefined') {
      this.channel = new BroadcastChannel(this.name)
      this.channel.onmessage = (event: MessageEvent<PollMessage>) => {
        const message = event.data
        if (!message) return
        if ('request' in message) {
          const latest = this.shared
          if (latest?.snapshot && this.recent(latest)) this.channel?.postMessage(latest)
        } else {
          this.receive(message)
        }
      }
    }
  }

  private receive(message: SharedPoll) {
    if (!message || !Number.isFinite(message.started)) return
    if (message.started < this.changedAt || message.started < (this.shared?.started ?? 0)) return
    const snapshot = message.snapshot ?? (message.etag === this.shared?.etag ? this.shared?.snapshot : undefined)
    this.shared = { ...message, snapshot }
  }

  changed() {
    this.changedAt = Date.now()
    this.shared = undefined
  }

  close() {
    this.channel?.close()
    this.channel = undefined
  }

  private recent(latest: SharedPoll) {
    return Date.now() - latest.started < sharedFreshness
  }

  private cached(base: SessionListSnapshot | undefined) {
    const latest = this.shared
    if (!latest || !this.recent(latest)) return
    return latest.snapshot ?? (base?.etag && base.etag === latest.etag ? base : undefined)
  }

  private async waitForShared(base: SessionListSnapshot | undefined, signal: AbortSignal) {
    const channel = this.channel
    if (!channel || this.cached(base) || signal.aborted) return
    await new Promise<void>(resolve => {
      const received = () => {
        if (this.cached(base)) finish()
      }
      const finish = () => {
        window.clearTimeout(timeout)
        signal.removeEventListener('abort', finish)
        channel.removeEventListener('message', received)
        resolve()
      }
      const timeout = window.setTimeout(finish, 40)
      channel.addEventListener('message', received)
      signal.addEventListener('abort', finish, { once: true })
      // The lock can be granted before the previous holder's broadcast arrives.
      channel.postMessage({ request: true })
    })
  }

  async refresh(base: SessionListSnapshot | undefined, signal: AbortSignal): Promise<SessionListSnapshot> {
    const poll = async () => {
      signal.throwIfAborted()
      const cached = this.cached(base)
      if (cached) return cached
      const started = Date.now()
      const result = await listSessions(this.projectId, base, signal)
      if (this.channel && !signal.aborted) {
        const message = { started, etag: result.etag, snapshot: result === base ? undefined : result }
        this.receive({ ...message, snapshot: result })
        this.channel.postMessage(message)
      }
      return result
    }
    if (this.channel && navigator.locks) {
      const immediate = await navigator.locks.request(this.name, { ifAvailable: true }, lock => lock ? poll() : undefined)
      if (immediate) return immediate
      return await navigator.locks.request(this.name, { signal }, async () => {
        await this.waitForShared(base, signal)
        return await poll()
      })
    }
    return await poll()
  }
}
