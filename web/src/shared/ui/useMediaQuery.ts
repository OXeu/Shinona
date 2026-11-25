import { onMounted, onUnmounted, ref } from 'vue'

export function useMediaQuery(query: string) {
  const matches = ref(false)
  let media: MediaQueryList | undefined

  function sync() {
    matches.value = media?.matches ?? false
  }

  onMounted(() => {
    media = window.matchMedia(query)
    sync()
    media.addEventListener('change', sync)
  })

  onUnmounted(() => media?.removeEventListener('change', sync))
  return matches
}
