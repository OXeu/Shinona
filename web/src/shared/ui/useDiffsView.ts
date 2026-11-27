import { onMounted, onUnmounted, shallowRef, watch } from 'vue'
import type { ShallowRef } from 'vue'

interface DiffsInstance {
  cleanUp: (recycle?: boolean) => void
}

/**
 * Mounts a Diffs renderer into a host element and keeps it in sync with the
 * reactive sources. Diffs renders into a shadow root it owns, so Vue must not
 * patch the host's children.
 */
export function useDiffsView<T extends DiffsInstance>(
  mount: (host: HTMLElement) => T,
  update: (instance: T) => void,
  sources: Array<() => unknown>,
): ShallowRef<HTMLElement | null> {
  const host = shallowRef<HTMLElement | null>(null)
  let instance: T | undefined

  function refresh() {
    if (instance) update(instance)
  }

  onMounted(() => {
    if (!host.value) return
    instance = mount(host.value)
    update(instance)
  })
  watch(sources, refresh, { flush: 'post' })
  onUnmounted(() => {
    instance?.cleanUp()
    instance = undefined
  })
  return host
}
