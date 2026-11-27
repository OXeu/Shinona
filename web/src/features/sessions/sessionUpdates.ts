import { HttpError } from '../../shared/http'
import { RetryBackoff } from '../../shared/retryBackoff'
import { sessionApi } from './sessionApi'
import type { AgentSession, Policy, Turn } from './sessionApi'
import { watchLiveSession } from './wire/liveSession'
import type { LiveCloseFailure } from './wire/liveSession'

const refreshInterval = 1_000
const liveReconnectInterval = 5_000
const hiddenGracePeriod = 5_000

interface SessionUpdateCallbacks {
  sync(entry: AgentSession, policy: Policy | null): void
  progress(turn: Turn): void
  missing(): Promise<void>
  unauthorized(): void
  failed(retrying: boolean, reason?: LiveCloseFailure): void
}

export class SessionUpdates {
  private readonly controller = new AbortController()
  private revision = 0
  private timer?: ReturnType<typeof setTimeout>
  private hiddenTimer?: ReturnType<typeof setTimeout>
  private fetching?: AbortController
  private live?: AbortController
  private liveRetryAt = 0
  private liveBlocked = false
  private readonly retry = new RetryBackoff()

  constructor(readonly id: string, private readonly callbacks: SessionUpdateCallbacks) {}

  get signal() {
    return this.controller.signal
  }

  invalidate() {
    this.revision++
  }

  close() {
    clearTimeout(this.hiddenTimer)
    this.cancelRefresh()
    this.live?.abort()
    this.controller.abort()
  }

  private cancelRefresh() {
    clearTimeout(this.timer)
    this.fetching?.abort()
    this.fetching = undefined
  }

  private signInRequired() {
    this.close()
    this.callbacks.unauthorized()
  }

  wake() {
    if (this.signal.aborted) return
    if (document.hidden) {
      this.cancelRefresh()
      if (this.live && this.hiddenTimer === undefined) {
        this.hiddenTimer = setTimeout(() => {
          this.hiddenTimer = undefined
          if (!document.hidden) return
          this.live?.abort()
          this.live = undefined
        }, hiddenGracePeriod)
      }
      return
    }
    clearTimeout(this.hiddenTimer)
    this.hiddenTimer = undefined
    this.retry.reset()
    void this.refresh()
  }

  retryNow() {
    this.liveBlocked = false
    this.liveRetryAt = 0
    this.wake()
  }

  schedule() {
    const delay = this.liveRetryAt ? Math.max(0, this.liveRetryAt - Date.now()) : refreshInterval
    this.scheduleAfter(delay)
  }

  private scheduleAfter(delay: number) {
    clearTimeout(this.timer)
    if (this.live || this.liveBlocked || document.hidden || this.signal.aborted) return
    this.timer = setTimeout(() => {
      void this.refresh()
    }, delay)
  }

  private startLive(entry: AgentSession) {
    if (document.hidden || this.signal.aborted) return
    const { signal } = this.live = new AbortController()
    void watchLiveSession(entry, {
      sync: (updated, settings) => this.callbacks.sync(updated, settings.selected),
      progress: this.callbacks.progress,
      unauthorized: () => this.signInRequired(),
      failed: (reason) => {
        this.live = undefined
        this.liveBlocked = true
        this.callbacks.failed(false, reason)
      },
      missing: () => {
        this.live = undefined
        this.liveBlocked = true
        void this.callbacks.missing().catch(() => {
          if (!this.signal.aborted) this.callbacks.failed(false)
        })
      },
    }, signal).catch(() => {
      if (signal.aborted) return
      this.live = undefined
      this.liveRetryAt = Date.now() + liveReconnectInterval
      this.schedule()
    })
  }

  async refresh() {
    if (this.fetching || this.live || this.liveBlocked || document.hidden || this.signal.aborted) return
    if (Date.now() < this.liveRetryAt) {
      this.schedule()
      return
    }
    this.liveRetryAt = 0
    clearTimeout(this.timer)
    const request = this.fetching = new AbortController()
    const { signal } = request
    const revision = this.revision
    try {
      const result = await sessionApi.detail(this.id, signal)
      if (signal.aborted || document.hidden) return
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
      if (signal.aborted || document.hidden) return
      if (error instanceof HttpError && error.status === 401) {
        this.signInRequired()
        return
      }
      const delay = this.retry.next()
      this.callbacks.failed(delay !== undefined)
      if (delay !== undefined) this.scheduleAfter(delay)
    } finally {
      if (this.fetching === request) this.fetching = undefined
    }
  }
}
