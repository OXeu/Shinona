<script setup lang="ts">
import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import { onMounted, onUnmounted, ref, useTemplateRef } from 'vue'
import '@xterm/xterm/css/xterm.css'

import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { UiBox } from '../../shared/ui/elements'
import { terminalApi } from './terminalApi'

const props = defineProps<{ sessionId: string; terminalId: string }>()
const surface = useTemplateRef<{ $el: HTMLElement }>('surface')
const reconnectable = ref(false)
const fit = new FitAddon()
const encoder = new TextEncoder()
let terminal: Terminal | undefined
let socket: WebSocket | undefined
let observer: ResizeObserver | undefined
let frame = 0
let ready = false

function sendSize() {
  if (!ready || socket?.readyState !== WebSocket.OPEN || !terminal) return
  socket.send(JSON.stringify({ cols: terminal.cols, rows: terminal.rows }))
}

function fitTerminal() {
  const dimensions = fit.proposeDimensions()
  if (!terminal || !dimensions || !surface.value?.$el.clientWidth || !surface.value.$el.clientHeight) return
  const cols = Math.min(500, Math.max(2, dimensions.cols))
  const rows = Math.min(300, Math.max(2, dimensions.rows))
  if (terminal.cols === cols && terminal.rows === rows) return
  terminal.resize(cols, rows)
  sendSize()
}

function sendInput(bytes: Uint8Array<ArrayBuffer>) {
  const ws = socket
  if (!ready || ws?.readyState !== WebSocket.OPEN) return
  if (ws.bufferedAmount + bytes.length > 512 * 1024) {
    notify.error(t('terminal.inputTooLarge'))
    return
  }
  for (let start = 0; start < bytes.length; start += 16_384) {
    ws.send(bytes.subarray(start, start + 16_384))
  }
}

function connect() {
  if (!terminal || socket) return
  fitTerminal()
  terminal.reset()
  reconnectable.value = false
  const ws = terminalApi.connect(props.sessionId, props.terminalId)
  socket = ws
  ws.binaryType = 'arraybuffer'
  ws.onmessage = ({ data }: MessageEvent<unknown>) => {
    if (socket !== ws || !terminal) return
    if (data instanceof ArrayBuffer) {
      terminal.write(new Uint8Array(data))
    } else if (data === '{"state":"running"}') {
      // Previous screen output can contain control queries. Enable replies
      // only after xterm has replayed that output.
      terminal.write('', () => {
        if (socket !== ws) return
        ready = true
        sendSize()
      })
    } else if (data === '{"state":"failed"}') {
      notify.error(t('terminal.unavailable'))
    }
  }
  ws.onclose = ({ code }) => {
    if (socket !== ws) return
    socket = undefined
    ready = false
    reconnectable.value = code !== 1000
    if (reconnectable.value) notify.error(t('terminal.unavailable'))
  }
}

defineExpose({ connect, reconnectable })

onMounted(() => {
  if (!surface.value) return
  const styles = getComputedStyle(surface.value.$el)
  const background = styles.getPropertyValue('--terminal-background').trim()
  const foreground = styles.getPropertyValue('--terminal-foreground').trim()
  terminal = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace',
    theme: { background, foreground, cursor: foreground },
  })
  terminal.loadAddon(fit)
  terminal.open(surface.value.$el)
  terminal.onData(data => sendInput(encoder.encode(data)))
  terminal.onBinary(data => sendInput(Uint8Array.from(data, char => char.charCodeAt(0))))
  observer = new ResizeObserver(() => {
    cancelAnimationFrame(frame)
    frame = requestAnimationFrame(fitTerminal)
  })
  observer.observe(surface.value.$el)
  connect()
})

onUnmounted(() => {
  observer?.disconnect()
  cancelAnimationFrame(frame)
  const ws = socket
  socket = undefined
  ws?.close()
  terminal?.dispose()
})
</script>

<template>
  <UiBox ref="surface" class="terminal-surface" />
</template>

<style lang="less" scoped>
.terminal-surface {
  flex: 1 1 0;
  min-height: 0;
  overflow: hidden;
  padding: 6px 10px;
  background: var(--terminal-background);
}
.terminal-surface :deep(.xterm) { height: 100%; }
</style>
