<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, useTemplateRef, watch } from 'vue'

const props = withDefaults(defineProps<{
  label: string
  align?: 'start' | 'end'
  boundary?: string
  bare?: boolean
  side?: 'top' | 'bottom'
  disabled?: boolean
  scroll?: boolean
  floating?: boolean
}>(), { align: 'end', boundary: '', side: 'bottom' })

const emit = defineEmits<{ 'open-change': [open: boolean] }>()

const root = useTemplateRef('root')
const summary = useTemplateRef('summary')
const menu = useTemplateRef('menu')
const insideDialog = ref(false)
const detached = computed(() => props.floating || insideDialog.value)
let dialog: HTMLDialogElement | null = null
let externalAnchor: HTMLElement | undefined
let clickOffset: number | undefined
let resizeObserver: ResizeObserver | undefined

function boundaryFor(dropdown: HTMLElement) {
  if (insideDialog.value) return document.documentElement
  const parent = dropdown.parentElement
  return (props.boundary && parent?.closest(props.boundary)) || parent
}

function isFloating(content: HTMLElement) {
  return detached.value && typeof content.showPopover === 'function'
}

function setPopover(open: boolean) {
  const content = menu.value
  if (!content || !isFloating(content) || open === content.matches(':popover-open')) return
  if (open) content.showPopover()
  else content.hidePopover()
}

function positionMenu() {
  const dropdown = root.value
  const trigger = externalAnchor ?? summary.value
  const content = menu.value
  if (!dropdown?.open || !trigger || !content) return
  const boundary = boundaryFor(dropdown)
  if (!boundary) return

  const boundaryLeft = boundary.getBoundingClientRect().left + boundary.clientLeft
  const start = Math.max(0, boundaryLeft)
  const end = Math.min(boundaryLeft + boundary.clientWidth, document.documentElement.clientWidth)
  content.style.setProperty('--dropdown-available-width', `${Math.max(0, end - start)}px`)

  const { left, right, top, bottom, width } = trigger.getBoundingClientRect()
  const { width: menuWidth, height: menuHeight } = content.getBoundingClientRect()
  const rtl = getComputedStyle(trigger).direction === 'rtl'
  const anchorOnRight = clickOffset === undefined ? (props.align === 'end') !== rtl : rtl
  const alignedEdge = anchorOnRight ? right : left
  const anchor = clickOffset === undefined ? alignedEdge : left + Math.min(clickOffset, width)
  const proposedLeft = anchor - (anchorOnRight ? menuWidth : 0)
  const menuLeft = Math.max(start, Math.min(proposedLeft, end - menuWidth))
  if (isFloating(content)) {
    const above = top - menuHeight - 6
    const below = bottom + 6
    const maxTop = Math.max(0, document.documentElement.clientHeight - menuHeight)
    const preferred = props.side === 'top' ? above : below
    const alternate = props.side === 'top' ? below : above
    const menuTop = preferred >= 0 && preferred <= maxTop ? preferred : alternate
    content.style.setProperty('--dropdown-left', `${menuLeft}px`)
    content.style.setProperty('--dropdown-top', `${Math.max(0, Math.min(menuTop, maxTop))}px`)
    return
  }
  const containingLeft = dropdown.getBoundingClientRect().left + dropdown.clientLeft
  content.style.setProperty('--dropdown-left', `${menuLeft - containingLeft}px`)
}

function syncPositioning() {
  resizeObserver?.disconnect()
  window.removeEventListener('resize', positionMenu)
  window.removeEventListener('scroll', positionMenu, true)
  document.removeEventListener('pointerdown', closeOutside)
  const dropdown = root.value
  emit('open-change', !!dropdown?.open)
  setPopover(!!dropdown?.open)
  if (!dropdown?.open) {
    dropdown?.querySelectorAll<HTMLDetailsElement>('details[open]').forEach((child) => { child.open = false })
    return
  }
  positionMenu()
  resizeObserver ??= new ResizeObserver(positionMenu)
  const boundary = boundaryFor(dropdown)
  if (boundary) resizeObserver.observe(boundary)
  resizeObserver.observe(dropdown)
  if (externalAnchor) resizeObserver.observe(externalAnchor)
  if (menu.value) resizeObserver.observe(menu.value)
  window.addEventListener('resize', positionMenu)
  document.addEventListener('pointerdown', closeOutside)
  if (detached.value) window.addEventListener('scroll', positionMenu, true)
}

function close(restoreFocus = false) {
  const trigger = externalAnchor ?? summary.value
  externalAnchor = undefined
  clickOffset = undefined
  if (root.value?.open) root.value.open = false
  setPopover(false)
  if (restoreFocus) trigger?.focus()
}

function openMenu(trigger: HTMLElement, clientX?: number) {
  const dropdown = root.value
  if (props.disabled || !dropdown) return
  clickOffset = clientX === undefined ? undefined : Math.max(0, clientX - trigger.getBoundingClientRect().left)
  dropdown.open = true
  setPopover(true)
  positionMenu()
}

function openFrom(trigger: HTMLElement, clientX?: number) {
  if (props.disabled) return
  externalAnchor = trigger
  openMenu(trigger, clientX)
  menu.value?.querySelector<HTMLElement>('button:not(:disabled)')?.focus()
}

defineExpose({ openFrom })

function toggleMenu(event: MouseEvent) {
  event.preventDefault()
  const dropdown = root.value
  if (props.disabled || !dropdown || !summary.value) return
  if (dropdown.open) {
    close()
    return
  }
  openMenu(summary.value, event.detail > 0 ? event.clientX : undefined)
}

function closeOutside(event: PointerEvent) {
  if (event.target instanceof Node && !root.value?.contains(event.target)) close()
}

function escape(event: KeyboardEvent) {
  if (!root.value?.open) return
  event.preventDefault()
  event.stopPropagation()
  close(true)
}

watch(() => props.disabled, (disabled) => {
  if (disabled) close()
})

function closeWithDialog() {
  close()
}

onMounted(() => {
  dialog = root.value?.closest('dialog') ?? null
  insideDialog.value = dialog !== null
  dialog?.addEventListener('close', closeWithDialog)
})

onUnmounted(() => {
  dialog?.removeEventListener('close', closeWithDialog)
  document.removeEventListener('pointerdown', closeOutside)
  window.removeEventListener('resize', positionMenu)
  window.removeEventListener('scroll', positionMenu, true)
  resizeObserver?.disconnect()
})
</script>

<template>
  <details
    ref="root"
    class="ui-dropdown"
    @toggle="syncPositioning"
    @keydown.esc="escape"
  >
    <summary
      ref="summary"
      class="dropdown-trigger"
      :aria-label="label"
      :aria-disabled="disabled || undefined"
      :title="label"
      @click="toggleMenu"
    >
      <slot name="trigger" />
    </summary>
    <div
      ref="menu"
      class="dropdown-content"
      :class="{ 'dropdown-content--scroll': scroll, 'dropdown-content--bare': bare }"
      :data-side="side"
      :popover="detached ? 'manual' : undefined"
    >
      <slot :close="close" />
    </div>
  </details>
</template>

<style lang="less" scoped>
.ui-dropdown { display: inline-block; position: relative; min-width: 0; }
.dropdown-trigger { cursor: pointer; list-style: none; }
.dropdown-trigger[aria-disabled='true'] { cursor: default; opacity: 0.55; }
.dropdown-trigger::-webkit-details-marker { display: none; }
.dropdown-content {
  position: absolute;
  z-index: var(--z-dropdown);
  width: max-content;
  left: var(--dropdown-left, 0px);
  min-width: min(10.5rem, var(--dropdown-available-width, 100vw));
  max-width: min(18rem, var(--dropdown-available-width, 100vw));
  overflow-wrap: anywhere;
  padding: 4px;
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--elevated);
  box-shadow: var(--shadow-md);
}
.dropdown-content--bare {
  min-width: 0;
  max-width: var(--dropdown-available-width, 100vw);
  padding: 0;
  border: 0;
  background: transparent;
  box-shadow: none;
  pointer-events: none;
}
.dropdown-content--scroll { max-height: min(18rem, 42dvh); overflow: auto; }
.dropdown-content[data-side='top'] { bottom: calc(100% + 6px); }
.dropdown-content[data-side='bottom'] { top: calc(100% + 6px); }
.dropdown-content:popover-open {
  position: fixed;
  inset: var(--dropdown-top, 0) auto auto var(--dropdown-left, 0);
  margin: 0;
}
</style>
