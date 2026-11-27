<script setup lang="ts">
import { themeTokens } from 'virtual:arrow-frame'
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'

import { languageTag, t } from '../i18n'
import { mountArrowFrame } from './arrowFrames'
import { UiButton } from './elements'

const props = defineProps<{ source: Record<string, string>; summary: string }>()
const container = ref<HTMLDivElement>()
const slot = ref<HTMLDivElement>()
const height = ref(0)
const failed = ref(false)
const inactive = ref(false)
const maxHeight = 960

let observer: IntersectionObserver | undefined
let activity: AbortController | undefined
let current: Awaited<ReturnType<typeof mountArrowFrame>>
let port: MessagePort | undefined
let timeout: number | undefined
let visible = false

function release() {
  window.clearTimeout(timeout)
  port?.close()
  port = undefined
  activity?.abort()
  activity = undefined
  current?.release()
  current = undefined
}

function fail() {
  failed.value = true
  inactive.value = false
  height.value = 0
  release()
}

function waitForSlot() {
  inactive.value = true
  height.value = 0
}

function revoke() {
  release()
  waitForSlot()
  if (visible) void show()
}

function activate() {
  const priority = !failed.value
  failed.value = false
  void show(priority)
}

async function show(priority = false) {
  if (!visible || failed.value || !slot.value) return
  if (activity) {
    if (!priority || !inactive.value) return
    release()
  }
  inactive.value = false
  const { signal } = activity = new AbortController()
  try {
    const lease = await mountArrowFrame({
      slot: slot.value, title: props.summary, signal, onWait: waitForSlot, onRevoke: revoke, priority,
    })
    if (signal.aborted) {
      lease?.release()
      return
    }
    if (!lease) return
    inactive.value = false
    current = lease
    lease.element.title = props.summary
    lease.element.height = String(height.value)
    lease.element.dataset.ready = 'false'
    timeout = window.setTimeout(fail, 10_000)
    await lease.ready
    if (signal.aborted) return
    const style = getComputedStyle(lease.element)
    const theme = Object.fromEntries(themeTokens.map(name => [name, style.getPropertyValue(name)]))
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
      height.value = Math.min(maxHeight, Math.max(1, Math.ceil(size)))
      lease.element.height = String(height.value)
      lease.element.dataset.ready = 'true'
    }
    lease.render({ source: { ...props.source }, theme, lang: languageTag() }, channel.port2)
  } catch {
    if (!signal.aborted) fail()
  }
}

watch(() => props.source, () => {
  release()
  failed.value = false
  inactive.value = false
  height.value = 0
  if (visible) void show()
}, { deep: true })

watch(() => props.summary, summary => {
  if (current) current.element.title = summary
})

onMounted(() => {
  if (!container.value) return
  observer = new IntersectionObserver(entries => {
    visible = entries.at(-1)?.isIntersecting === true
    if (visible) void show()
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
    <span :hidden="!failed && !inactive && height > 0">{{ summary }}</span>
    <UiButton :hidden="!inactive && !failed" @click="activate">
      {{ t(failed ? 'common.retry' : 'chat.activateRichContent') }}
    </UiButton>
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
