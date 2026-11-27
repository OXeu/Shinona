<script setup lang="ts">
import { onUnmounted, ref, useId, useTemplateRef, watch } from 'vue'

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

const dividerSize = 5
const slideDuration = 220

const size = defineModel<number>({ required: true })
const root = useTemplateRef('root')
const startId = useId()
const animating = ref(false)
const startPaneSize = ref<string>()
const endPaneSize = ref<string>()
let drag: { id: number; bounds: DOMRect; offset: number } | null = null
let animationTimer: number | undefined

function length(bounds: DOMRect) {
  return props.direction === 'row' ? bounds.width : bounds.height
}

function position(event: PointerEvent, bounds: DOMRect) {
  return props.direction === 'row' ? event.clientX - bounds.left : event.clientY - bounds.top
}

function stopAnimation() {
  window.clearTimeout(animationTimer)
  animating.value = false
  startPaneSize.value = undefined
  endPaneSize.value = undefined
}

function openStartLength(total: number) {
  const min = Math.min(props.startMin, total * 0.5)
  const max = Math.max(total * 0.5, total - props.endMin - dividerSize)
  return Math.max(min, Math.min(max, total * size.value / 100))
}

watch(
  () => [!!props.startCollapsed, !!props.endCollapsed] as const,
  ([nextStart, nextEnd], [prevStart, prevEnd]) => {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      stopAnimation()
      return
    }
    const total = root.value ? length(root.value.getBoundingClientRect()) : 0
    if (total > 0) {
      const openStart = openStartLength(total)
      const openEnd = Math.max(0, total - openStart - dividerSize)
      startPaneSize.value = nextStart !== prevStart
        ? (nextEnd || prevEnd ? total : openStart) + 'px'
        : undefined
      endPaneSize.value = nextEnd !== prevEnd
        ? (nextStart || prevStart ? total : openEnd) + 'px'
        : undefined
    }
    animating.value = true
    window.clearTimeout(animationTimer)
    animationTimer = window.setTimeout(stopAnimation, slideDuration + 30)
  },
)

function resize(percent: number, available: number) {
  if (available <= 0) return
  const min = Math.min(50, 100 * props.startMin / available)
  const max = Math.max(50, 100 - 100 * props.endMin / available)
  size.value = Math.round(Math.max(min, Math.min(max, percent)) * 10) / 10
}

function start(event: PointerEvent) {
  if (event.button !== 0 || !root.value || !(event.currentTarget instanceof HTMLElement)) return
  stopAnimation()
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

onUnmounted(stopAnimation)
</script>

<template>
  <div
    ref="root"
    class="split-panel"
    :class="'split-panel--' + direction"
    :data-animating="animating || undefined"
    :style="{
      '--split-size': size + '%',
      '--split-start-min': startMin + 'px',
      '--split-end-min': endMin + 'px',
    }"
  >
    <div
      :id="startId"
      class="split-start"
      :data-collapsed="startCollapsed || undefined"
      :inert="startCollapsed"
      :aria-hidden="startCollapsed || undefined"
      :style="startPaneSize ? { '--pane-size': startPaneSize } : undefined"
    >
      <div class="split-pane-inner">
        <slot name="start" />
      </div>
    </div>
    <div
      class="split-divider"
      role="separator"
      :tabindex="startCollapsed || endCollapsed ? -1 : 0"
      :data-collapsed="(startCollapsed || endCollapsed) || undefined"
      :inert="startCollapsed || endCollapsed"
      :aria-hidden="(startCollapsed || endCollapsed) || undefined"
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
    <div
      class="split-end"
      :data-collapsed="endCollapsed || undefined"
      :inert="endCollapsed"
      :aria-hidden="endCollapsed || undefined"
      :style="endPaneSize ? { '--pane-size': endPaneSize } : undefined"
    >
      <div class="split-pane-inner">
        <slot name="end" />
      </div>
    </div>
  </div>
</template>

<style lang="less" scoped>
.split-panel {
  --divider-size: 5px;
  --split-start: clamp(
    min(var(--split-start-min), 50%),
    var(--split-size),
    max(50%, calc(100% - var(--split-end-min) - var(--divider-size)))
  );
  --split-duration: 220ms;
  --split-ease: cubic-bezier(0.22, 1, 0.36, 1);
  display: flex;
  width: 100%;
  height: 100%;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
}

.split-panel--column { flex-direction: column; }

.split-start,
.split-end {
  display: flex;
  min-width: 0;
  min-height: 0;
  overflow: hidden;
}

.split-panel--column > .split-start,
.split-panel--column > .split-end {
  flex-direction: column;
}

.split-start {
  justify-content: flex-end;
  flex: 0 0 var(--split-start);
}

.split-end {
  justify-content: flex-start;
  flex: 0 0 calc(100% - var(--split-start) - var(--divider-size));
}

.split-start[data-collapsed='true'],
.split-end[data-collapsed='true'] {
  flex-basis: 0px;
  visibility: hidden;
}

.split-start:has(~ .split-end[data-collapsed='true']):not([data-collapsed='true']),
.split-start[data-collapsed='true'] ~ .split-end:not([data-collapsed='true']) {
  flex-basis: 100%;
}

.split-pane-inner {
  flex: 0 0 auto;
  width: var(--pane-size, 100%);
  height: 100%;
  min-width: 0;
  min-height: 0;
  opacity: 1;
}

.split-panel--column > :is(.split-start, .split-end) > .split-pane-inner {
  width: 100%;
  height: var(--pane-size, 100%);
}

.split-start[data-collapsed='true'] > .split-pane-inner,
.split-end[data-collapsed='true'] > .split-pane-inner {
  opacity: 0;
}

.split-divider {
  position: relative;
  flex: 0 0 var(--divider-size);
  z-index: 2;
  cursor: col-resize;
  touch-action: none;
  outline: none;
}

.split-divider[data-collapsed='true'] {
  flex-basis: 0px;
  opacity: 0;
  visibility: hidden;
  pointer-events: none;
}

.split-panel--column > .split-divider { cursor: row-resize; }
.split-divider::after {
  position: absolute;
  inset: 0 2px;
  background: var(--border);
  content: '';
}
.split-panel--column > .split-divider::after { inset: 2px 0; }
.split-divider:is(:hover, :active)::after { inset: 0; background: var(--border-hover); }
.split-divider:focus-visible::after { inset: 0; background: var(--accent); opacity: .72; }

.split-panel[data-animating='true'] > :is(.split-start, .split-end) {
  transition:
    flex-basis var(--split-duration) var(--split-ease),
    visibility 0s linear 0s;
}

.split-panel[data-animating='true'] > :is(.split-start, .split-end)[data-collapsed='true'] {
  transition:
    flex-basis var(--split-duration) var(--split-ease),
    visibility 0s linear var(--split-duration);
}

.split-panel[data-animating='true'] > :is(.split-start, .split-end) > .split-pane-inner {
  transition: opacity 180ms ease;
}

.split-panel[data-animating='true'] > .split-divider {
  transition:
    flex-basis var(--split-duration) var(--split-ease),
    opacity 160ms ease,
    visibility 0s linear 0s;
}

.split-panel[data-animating='true'] > .split-divider[data-collapsed='true'] {
  transition:
    flex-basis var(--split-duration) var(--split-ease),
    opacity 140ms ease,
    visibility 0s linear var(--split-duration);
}

@media (prefers-reduced-motion: reduce) {
  .split-panel[data-animating='true'] > :is(.split-start, .split-end, .split-divider),
  .split-panel[data-animating='true'] > :is(.split-start, .split-end) > .split-pane-inner {
    transition: none;
  }
}
</style>
