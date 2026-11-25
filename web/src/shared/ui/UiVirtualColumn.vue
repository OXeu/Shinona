<script setup lang="ts" generic="T">
import { useVirtualizer } from '@tanstack/vue-virtual'
import { computed, onUnmounted, useTemplateRef, watch } from 'vue'

const props = defineProps<{
  label: string
  items: T[]
  itemKey: (item: T) => string
  bottomInset: number
}>()
defineSlots<{ default(props: { item: T }): unknown; empty(): unknown }>()

const scroll = useTemplateRef('scroll')
const virtualizer = useVirtualizer<HTMLElement, HTMLElement>(computed(() => {
  const items = props.items
  return {
    count: items.length,
    getScrollElement: () => scroll.value,
    getItemKey: (index) => items[index] === undefined ? index : props.itemKey(items[index]),
    estimateSize: () => 100,
    overscan: 6,
    gap: 14,
    paddingStart: 30,
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
