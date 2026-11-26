const maxFrames = 3

type ReleaseSlot = () => void

export class ArrowFrameSlots {
  private readonly active = new Map<ReleaseSlot, () => void>()
  private readonly waiting = new Set<() => void>()

  reserve(
    signal: AbortSignal, onWait: () => void, onRevoke: () => void, priority: boolean,
  ): Promise<ReleaseSlot | undefined> {
    if (signal.aborted) return Promise.resolve(undefined)
    return new Promise(resolve => {
      const release = () => {
        if (this.active.delete(release)) this.waiting.values().next().value?.()
      }
      const grant = () => {
        this.waiting.delete(grant)
        signal.removeEventListener('abort', cancel)
        this.active.set(release, onRevoke)
        resolve(release)
      }
      const cancel = () => {
        this.waiting.delete(grant)
        resolve(undefined)
      }

      if (this.active.size < maxFrames) {
        grant()
        return
      }
      const oldest = priority ? this.active.entries().next().value : undefined
      if (oldest) {
        const [previous, revoke] = oldest
        this.active.delete(previous)
        grant()
        revoke()
        return
      }
      this.waiting.add(grant)
      signal.addEventListener('abort', cancel, { once: true })
      onWait()
    })
  }
}
