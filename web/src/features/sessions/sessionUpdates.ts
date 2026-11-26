import { HttpError } from '../../shared/http'
import { RetryBackoff } from '../../shared/retryBackoff'
import { sessionApi } from './sessionApi'
import type { AgentSession, Policy, Turn } from './sessionApi'
import { watchLiveSession } from './wire/liveSession'

const activeRefreshInterval = 1_000
const liveReconnectInterval = 5_000

interface SessionUpdateCallbacks {
  active(): boolean
  sync(entry: AgentSession, policy: Policy | null): void
  progress(turn: Turn): void
  missing(): Promise<void>
  unauthorized(): void
  failed(retrying: boolean): void
}

export class SessionUpdates {
  private readonly controller = new AbortController()
  private revision = 0
  private timer?: ReturnType<typeof setTimeout>
  private fetching = false
  private live = false
  private liveRetryAt = 0
  private readonly retry = new RetryBackoff()

  constructor(readonly id: string, private readonly callbacks: SessionUpdateCallbacks) {}

  get signal() {
    return this.controller.signal
  }

  invalidate() {
    this.revision++
  }

  close() {
    clearTimeout(this.timer)
    this.controller.abort()
  }

  wake() {
    clearTimeout(this.timer)
    if (document.hidden) return
    this.retry.reset()
    void this.refresh()
  }

  schedule() {
    const delay = this.callbacks.active()
      ? activeRefreshInterval
      : Math.max(activeRefreshInterval, this.liveRetryAt - Date.now())
    this.scheduleAfter(delay)
  }

  private scheduleAfter(delay: number) {
    clearTimeout(this.timer)
    if (this.live || document.hidden || this.signal.aborted) return
    this.timer = setTimeout(() => {
      void this.refresh()
    }, delay)
  }

  private startLive(entry: AgentSession) {
    if (document.hidden || Date.now() < this.liveRetryAt) return
    this.live = true
    const { signal } = this
    void watchLiveSession(entry, {
      sync: (updated, settings) => this.callbacks.sync(updated, settings.selected),
      progress: this.callbacks.progress,
      missing: () => {
        void this.callbacks.missing().catch(() => {
          if (!signal.aborted) this.callbacks.failed(false)
        })
      },
    }, signal).catch(() => {
      if (signal.aborted) return
      this.live = false
      this.liveRetryAt = Date.now() + liveReconnectInterval
      this.schedule()
    })
  }

  async refresh() {
    const { signal } = this
    if (this.fetching || this.live || document.hidden || signal.aborted) return
    clearTimeout(this.timer)
    this.fetching = true
    const revision = this.revision
    try {
      const result = await sessionApi.detail(this.id, signal)
      if (signal.aborted) return
      this.retry.reset()
      if (revision !== this.revision) {
        this.schedule()
        return
      }
      if (!result.session) {
        await this.callbacks.missing()
        return
      }
      this.callbacks.sync(result.session, result.sandboxSettings.selected)
      this.startLive(result.session)
      this.schedule()
    } catch (error) {
      if (signal.aborted) return
      if (error instanceof HttpError && error.status === 401) {
        this.close()
        this.callbacks.unauthorized()
        return
      }
      const delay = this.retry.next()
      this.callbacks.failed(delay !== undefined)
      if (delay !== undefined) this.scheduleAfter(delay)
    } finally {
      this.fetching = false
    }
  }
}
