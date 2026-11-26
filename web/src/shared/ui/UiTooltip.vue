<script setup lang="ts">
import { computed, ref, useId, useTemplateRef, watch } from 'vue'

import UiIcon from './UiIcon.vue'

defineProps<{ label: string; text: string }>()

const id = useId()
const trigger = useTemplateRef('trigger')
const popup = useTemplateRef('popup')
const hovered = ref(false)
const focused = ref(false)
const dismissed = ref(false)
const position = ref({ left: 0, top: 0 })
const above = ref(false)
const open = computed(() => !dismissed.value && (hovered.value || focused.value))

function enter(event: PointerEvent) {
  if (event.pointerType === 'touch') return
  hovered.value = true
  dismissed.value = false
}

function focus() {
  focused.value = true
  dismissed.value = false
}

function place() {
  if (!trigger.value || !popup.value) return
  const anchor = trigger.value.getBoundingClientRect()
  const { offsetWidth, offsetHeight } = popup.value
  const viewport = document.documentElement
  const inset = 8
  above.value = anchor.bottom + offsetHeight > viewport.clientHeight - inset &&
    anchor.top >= offsetHeight + inset
  position.value = {
    left: Math.max(inset, Math.min(anchor.left, viewport.clientWidth - offsetWidth - inset)),
    top: above.value ? anchor.top - offsetHeight : anchor.bottom,
  }
}

function escape(event: KeyboardEvent) {
  if (event.key !== 'Escape' || event.defaultPrevented) return
  dismissed.value = true
  event.preventDefault()
}

watch(open, (show, _, onCleanup) => {
  if (!show) return
  place()
  window.addEventListener('resize', place)
  window.addEventListener('scroll', place, true)
  document.addEventListener('keydown', escape)
  onCleanup(() => {
    window.removeEventListener('resize', place)
    window.removeEventListener('scroll', place, true)
    document.removeEventListener('keydown', escape)
  })
}, { flush: 'post' })
</script>

<template>
  <span class="ui-tooltip" @pointerenter="enter" @pointerleave="hovered = false">
    <button
      ref="trigger"
      class="icon-button tooltip-trigger"
      type="button"
      :aria-label="label"
      :aria-describedby="id"
      @focus="focus"
      @blur="focused = false"
      @click="trigger?.focus(); focus()"
    >
      <UiIcon name="question-line" />
    </button>
    <span
      :id="id"
      ref="popup"
      role="tooltip"
      class="tooltip-popup"
      :data-above="above"
      :hidden="!open"
      :style="{ left: position.left + 'px', top: position.top + 'px' }"
    >
      <span class="tooltip-text">{{ text }}</span>
    </span>
  </span>
</template>

<style lang="less" scoped>
.ui-tooltip { display: inline-flex; flex: none; }
.tooltip-trigger { min-width: 24px; height: 24px; padding: 2px; font-size: 1rem; }
.tooltip-popup {
  position: fixed;
  z-index: var(--z-dropdown);
  width: max-content;
  max-width: min(20rem, calc(100vw - 16px));
  padding-top: 4px;
}
.tooltip-popup[data-above='true'] { padding-top: 0; padding-bottom: 4px; }
.tooltip-text {
  display: block;
  padding: 6px 9px;
  border: 1px solid var(--border-hover);
  border-radius: var(--radius-sm);
  color: var(--foreground);
  background: var(--elevated);
  box-shadow: var(--shadow-md);
  font-size: 0.8125rem;
  white-space: pre-line;
  overflow-wrap: anywhere;
}
</style>
