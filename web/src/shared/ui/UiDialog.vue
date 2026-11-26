<script setup lang="ts">
import { onBeforeUnmount, useTemplateRef, watch } from 'vue'

import { useDialogNotificationHost } from '../notifications'

const props = defineProps<{ label: string; busy?: boolean }>()
const open = defineModel<boolean>({ required: true })
const dialog = useTemplateRef('dialog')
let releaseHost: (() => void) | undefined

watch([open, dialog], ([value, element]) => {
  if (!element) return
  if (value && !element.open) {
    element.showModal()
    releaseHost = useDialogNotificationHost(element)
  } else if (!value && element.open) {
    releaseHost?.()
    releaseHost = undefined
    element.close()
  }
}, { flush: 'post' })

function cancel(event: Event) {
  if (props.busy) event.preventDefault()
}

function closed() {
  if (dialog.value?.open) return
  releaseHost?.()
  releaseHost = undefined
  open.value = false
}

onBeforeUnmount(() => releaseHost?.())
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
