<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'

import { languageTag } from '../i18n'
import { borrowArrowFrame } from './arrowFramePool'

const props = defineProps<{ source: Record<string, string>; summary: string }>()
const container = ref<HTMLDivElement>()
const slot = ref<HTMLDivElement>()
const height = ref(0)
const failed = ref(false)
const tokens = ['--background', '--surface', '--sidebar-bg', '--foreground', '--muted', '--primary', '--on-primary', '--accent',
  '--border', '--success', '--warning', '--danger', '--radius-sm', '--radius-md', '--radius-lg']

let observer: IntersectionObserver | undefined
let activity: AbortController | undefined
let current: Awaited<ReturnType<typeof borrowArrowFrame>>
let port: MessagePort | undefined
let timeout: number | undefined

function release(reuse = true) {
  window.clearTimeout(timeout)
  port?.close()
  activity?.abort()
  activity = undefined
  current?.release(reuse)
  current = undefined
}

function fail() {
  failed.value = true
  height.value = 0
  release(false)
}

async function show() {
  if (activity || failed.value || !slot.value) return
  const { signal } = activity = new AbortController()
  try {
    const lease = await borrowArrowFrame(slot.value, props.summary, signal)
    if (signal.aborted) {
      lease?.release(false)
      return
    }
    if (!lease) return
    current = lease
    lease.element.height = String(height.value)
    lease.element.dataset.ready = 'false'
    timeout = window.setTimeout(fail, 10_000)
    await lease.ready
    if (signal.aborted) return
    const style = getComputedStyle(lease.element)
    const theme = Object.fromEntries(tokens.map(name => [name, style.getPropertyValue(name)]))
    theme['--widget-font'] = `0.875rem/1.5 ${style.fontFamily}`
    const channel = new MessageChannel()
    port = channel.port1
    port.onmessage = (event: MessageEvent<{ type: string; height?: number }>) => {
      if (signal.aborted) return
      if (event.data.type === 'error') {
        fail()
        return
      }
      const size = event.data.height
      if (event.data.type !== 'resize' || typeof size !== 'number' || !Number.isFinite(size)) return
      window.clearTimeout(timeout)
      height.value = Math.max(1, Math.ceil(size))
      lease.element.height = String(height.value)
      lease.element.dataset.ready = 'true'
    }
    lease.render({ source: { ...props.source }, theme, lang: languageTag() }, channel.port2)
  } catch {
    if (!signal.aborted) fail()
  }
}

onMounted(() => {
  if (!container.value) return
  observer = new IntersectionObserver(entries => {
    if (entries.at(-1)?.isIntersecting) void show()
    else release()
  })
  observer.observe(container.value)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  release()
})
</script>

<template>
  <div ref="container">
    <div ref="slot" class="arrow-slot" :style="{ height: height ? height + 'px' : undefined }" />
    <span :hidden="!failed && height > 0">{{ summary }}</span>
  </div>
</template>

<style scoped lang="less">
.arrow-slot :deep(.arrow-view) {
  display: block;
  width: 100%;
  border: 0;
}
.arrow-slot :deep(.arrow-view[data-ready='false']) { visibility: hidden; }
</style>
