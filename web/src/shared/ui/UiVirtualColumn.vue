<script setup lang="ts" generic="T">
import { useVirtualizer } from '@tanstack/vue-virtual'
import { computed, nextTick, onUnmounted, ref, useTemplateRef, watch } from 'vue'

const props = defineProps<{
  label: string
  items: T[]
  itemKey: (item: T) => string
  bottomInset: number
}>()
defineSlots<{ default(props: { item: T }): unknown; start?(): unknown; empty(): unknown }>()

const scroll = useTemplateRef('scroll')
const start = useTemplateRef('start')
const startHeight = ref(0)
const prependPadding = ref(0)
const startPadding = computed(() => 30 + startHeight.value)
watch(start, (element, _, onCleanup) => {
  if (!element) return
  const measureStart = () => { startHeight.value = element.offsetHeight }
  const observer = new ResizeObserver(measureStart)
  measureStart()
  observer.observe(element)
  onCleanup(() => observer.disconnect())
})
const virtualizer = useVirtualizer<HTMLElement, HTMLElement>(computed(() => {
  const items = props.items
  return {
    count: items.length,
    getScrollElement: () => scroll.value,
    getItemKey: (index) => items[index] === undefined ? index : props.itemKey(items[index]),
    estimateSize: () => 100,
    overscan: 6,
    gap: 14,
    paddingStart: startPadding.value,
    scrollPaddingStart: prependPadding.value,
    paddingEnd: props.bottomInset,
    anchorTo: 'end',
    followOnAppend: true,
    scrollEndThreshold: 72,
  }
}))
const visible = computed(() => virtualizer.value.getVirtualItems().flatMap((row) => {
  const item = props.items[row.index]
  return item === undefined ? [] : [{ row, item }]
}))
const height = computed(() => virtualizer.value.getTotalSize())
const viewportHeight = computed(() => virtualizer.value.scrollRect?.height ?? 0)
let initialScrollDone = false
let frame: number | undefined

watch(() => props.items, (items, previous) => {
  const offset = scroll.value?.scrollTop
  const first = previous[0]
  if (!first || offset === undefined || !startHeight.value || offset >= startPadding.value) return
  const anchor = props.itemKey(first)
  const index = items.findIndex((item) => props.itemKey(item) === anchor)
  if (index <= 0) return
  // The start control is above the measured rows. Anchor to the first message
  // explicitly when it is below the viewport's top edge during a prepend.
  prependPadding.value = startPadding.value - offset
  void nextTick(() => {
    const currentIndex = props.items.findIndex((item) => props.itemKey(item) === anchor)
    if (currentIndex > 0) virtualizer.value.scrollToIndex(currentIndex, { align: 'start' })
  })
}, { flush: 'sync' })

function measure(value: unknown) {
  if (value instanceof HTMLElement) virtualizer.value.measureElement(value)
}

watch([height, viewportHeight], () => {
  if (initialScrollDone || frame !== undefined || !viewportHeight.value || !props.items.length) return
  frame = requestAnimationFrame(() => {
    frame = undefined
    if (!scroll.value?.clientHeight) return
    initialScrollDone = true
    virtualizer.value.scrollToEnd()
  })
}, { flush: 'post', immediate: true })
onUnmounted(() => {
  if (frame !== undefined) cancelAnimationFrame(frame)
})
</script>

<template>
  <div
    ref="scroll"
    class="chat-scroll"
    tabindex="0"
    :aria-label="label"
  >
    <div class="chat-empty" :hidden="items.length > 0">
      <slot name="empty" />
    </div>
    <div class="chat-virtual-list" :hidden="!items.length" :style="{ height: height + 'px' }">
      <div ref="start" class="virtual-column-start">
        <slot name="start" />
      </div>
      <div
        v-for="{ row, item } in visible"
        :key="String(row.key)"
        :ref="measure"
        class="chat-virtual-row"
        :data-index="row.index"
        :style="{ transform: 'translateY(' + row.start + 'px)' }"
      >
        <slot :item="item" />
      </div>
    </div>
  </div>
</template>

<style lang="less" scoped>
.virtual-column-start {
  position: absolute;
  inset: 0 0 auto;
}
</style>
