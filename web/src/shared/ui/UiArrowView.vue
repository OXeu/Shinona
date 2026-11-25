<script setup lang="ts">
import { onUnmounted, ref } from 'vue'

import runtime from 'virtual:arrow-frame'
import { languageTag } from '../i18n'

const props = defineProps<{ source: Record<string, string>; summary: string }>()
const height = ref(0)
const failed = ref(false)
const nonce = Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')
const csp = `default-src 'none'; script-src 'nonce-${nonce}' 'wasm-unsafe-eval'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'`
const documentSource = `<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${csp}">
<style>
  html { color-scheme: light dark; overflow-y: hidden }
  body { margin: 0; color: var(--foreground); font: var(--widget-font); overflow-wrap: anywhere }
  arrow-sandbox { display: flow-root }
</style>
</head><body><script nonce="${nonce}" type="module">${runtime}</${'script'}></body></html>`
const tokens = ['--background', '--surface', '--sidebar-bg', '--foreground', '--muted', '--primary', '--on-primary',
  '--border', '--success', '--warning', '--danger', '--radius-sm', '--radius-md', '--radius-lg']
let port: MessagePort | undefined
const timeout = window.setTimeout(fail, 10_000)

function fail() {
  window.clearTimeout(timeout)
  port?.close()
  failed.value = true
}

function connect(event: Event) {
  const target = event.currentTarget as HTMLIFrameElement
  const child = target?.contentWindow
  if (!target || !child || failed.value) return
  if (port) {
    fail()
    return
  }
  const channel = new MessageChannel()
  port = channel.port1
  port.onmessage = (event: MessageEvent<{ type: string; height?: number }>) => {
    if (event.data.type === 'error') {
      fail()
      return
    }
    const size = event.data.height
    if (event.data.type !== 'resize' || typeof size !== 'number' || !Number.isFinite(size)) return
    window.clearTimeout(timeout)
    height.value = Math.max(1, Math.ceil(size))
  }
  const style = getComputedStyle(target)
  const theme = Object.fromEntries(tokens.map(name => [name, style.getPropertyValue(name)]))
  theme['--widget-font'] = `0.875rem/1.5 ${style.fontFamily}`
  child.postMessage({ source: { ...props.source }, theme, lang: languageTag() }, '*', [channel.port2])
}

onUnmounted(() => {
  window.clearTimeout(timeout)
  port?.close()
})
</script>

<template>
  <iframe
    v-for="(content, index) in failed ? [] : [documentSource]"
    :key="index"
    class="arrow-view"
    :title="summary"
    :srcdoc.prop="content"
    sandbox="allow-scripts"
    referrerpolicy="no-referrer"
    :height="height"
    :data-ready="height > 0"
    @load="connect"
  />
  <span :hidden="!failed && height > 0">{{ summary }}</span>
</template>

<style scoped lang="less">
.arrow-view {
  display: block;
  width: 100%;
  border: 0;
}
.arrow-view[data-ready='false'] { visibility: hidden; }
</style>
