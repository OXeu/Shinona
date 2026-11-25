<script setup lang="ts">
import { onBeforeUnmount, useTemplateRef, watch } from 'vue'

const props = defineProps<{ label: string; busy?: boolean }>()
const open = defineModel<boolean>({ required: true })
const dialog = useTemplateRef('dialog')
let toaster: Element | null = null
let previousToastHost: Element | null = null

function restoreToaster() {
  if (toaster?.parentElement === dialog.value) {
    const host = previousToastHost?.isConnected && previousToastHost.matches('dialog[open]')
      ? previousToastHost
      : document.body
    if (toaster) host.append(toaster)
  }
  toaster = null
  previousToastHost = null
}

watch(open, (value) => {
  const element = dialog.value
  if (!element) return
  if (value && !element.open) {
    element.showModal()
    toaster = document.querySelector('.tst-viewport')
    if (toaster) {
      previousToastHost = toaster.parentElement
      element.append(toaster)
    }
  } else if (!value && element.open) {
    restoreToaster()
    element.close()
  }
}, { flush: 'post' })

function cancel(event: Event) {
  if (props.busy) event.preventDefault()
}

function closed() {
  if (dialog.value?.open) return
  restoreToaster()
  open.value = false
}

onBeforeUnmount(restoreToaster)
</script>

<template>
  <dialog
    ref="dialog"
    class="ui-dialog"
    :aria-label="label"
    @cancel="cancel"
    @close="closed"
  >
    <slot />
  </dialog>
</template>
