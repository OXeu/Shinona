<script setup lang="ts">
import { useId, useTemplateRef } from 'vue'

const props = withDefaults(defineProps<{
  direction?: 'row' | 'column'
  startMin?: number
  endMin?: number
  startCollapsed?: boolean
  endCollapsed?: boolean
  label: string
}>(), {
  direction: 'row',
  startMin: 0,
  endMin: 0,
})

const size = defineModel<number>({ required: true })
const root = useTemplateRef('root')
const startId = useId()
let drag: { id: number; bounds: DOMRect; offset: number } | null = null

function length(bounds: DOMRect) {
  return props.direction === 'row' ? bounds.width : bounds.height
}

function position(event: PointerEvent, bounds: DOMRect) {
  return props.direction === 'row' ? event.clientX - bounds.left : event.clientY - bounds.top
}

function resize(percent: number, available: number) {
  if (available <= 0) return
  const min = Math.min(50, 100 * props.startMin / available)
  const max = Math.max(50, 100 - 100 * props.endMin / available)
  size.value = Math.round(Math.max(min, Math.min(max, percent)) * 10) / 10
}

function start(event: PointerEvent) {
  if (event.button !== 0 || !root.value || !(event.currentTarget instanceof HTMLElement)) return
  const bounds = root.value.getBoundingClientRect()
  const handle = event.currentTarget.getBoundingClientRect()
  const handleStart = props.direction === 'row' ? handle.left - bounds.left : handle.top - bounds.top
  drag = { id: event.pointerId, bounds, offset: position(event, bounds) - handleStart }
  event.currentTarget.focus({ preventScroll: true })
  event.currentTarget.setPointerCapture(event.pointerId)
  event.preventDefault()
}

function move(event: PointerEvent) {
  if (!drag || event.pointerId !== drag.id) return
  const available = length(drag.bounds)
  resize(100 * (position(event, drag.bounds) - drag.offset) / available, available)
}

function end(event: PointerEvent) {
  if (drag?.id === event.pointerId) drag = null
}

function keydown(event: KeyboardEvent) {
  if (!root.value) return
  const decrease = props.direction === 'row' ? 'ArrowLeft' : 'ArrowUp'
  const increase = props.direction === 'row' ? 'ArrowRight' : 'ArrowDown'
  const targets: Record<string, number> = { Home: 0, End: 100, [decrease]: size.value - 2, [increase]: size.value + 2 }
  const target = targets[event.key]
  if (typeof target !== 'number') return
  event.preventDefault()
  resize(target, length(root.value.getBoundingClientRect()))
}
</script>

<template>
  <div
    ref="root"
    class="split-panel"
    :class="'split-panel--' + direction"
    :style="{
      '--split-size': size + '%',
      '--split-start-min': startMin + 'px',
      '--split-end-min': endMin + 'px',
    }"
  >
    <div
      :id="startId"
      class="split-start"
      :hidden="startCollapsed"
    >
      <slot name="start" />
    </div>
    <div
      class="split-divider"
      role="separator"
      tabindex="0"
      :hidden="startCollapsed || endCollapsed"
      :aria-label="label"
      :aria-controls="startId"
      :aria-orientation="direction === 'row' ? 'vertical' : 'horizontal'"
      :aria-valuenow="Math.round(size)"
      aria-valuemin="0"
      aria-valuemax="100"
      @pointerdown="start"
      @pointermove="move"
      @lostpointercapture="end"
      @pointercancel="end"
      @keydown="keydown"
    />
    <div class="split-end" :hidden="endCollapsed">
      <slot name="end" />
    </div>
  </div>
</template>

<style lang="less" scoped>
.split-panel {
  --divider-size: 5px;
  display: flex;
  width: 100%;
  height: 100%;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
}

.split-panel--column { flex-direction: column; }
.split-start, .split-end { min-width: 0; min-height: 0; overflow: hidden; }
.split-start {
  flex: 0 0 clamp(min(var(--split-start-min), 50%), var(--split-size), max(50%, calc(100% - var(--split-end-min) - var(--divider-size))));
}
.split-start:has(~ .split-end[hidden]) { flex: 1 1 0; }
.split-end { flex: 1 1 0; }
.split-divider {
  position: relative;
  flex: 0 0 var(--divider-size);
  z-index: 2;
  cursor: col-resize;
  touch-action: none;
  outline: none;
}
.split-panel--column > .split-divider { cursor: row-resize; }
.split-divider::after {
  position: absolute;
  inset: 0 2px;
  background: var(--border);
  content: '';
}
.split-panel--column > .split-divider::after { inset: 2px 0; }
.split-divider:is(:hover, :active, :focus-visible)::after { inset: 0; background: var(--primary); opacity: .48; }
</style>
