<script setup lang="ts">
import { useTemplateRef, watch } from 'vue'

defineProps<{ label: string }>()
const open = defineModel<boolean>({ required: true })
const dialog = useTemplateRef('dialog')

watch(open, (value) => {
  if (value && !dialog.value?.open) dialog.value?.showModal()
  if (!value && dialog.value?.open) dialog.value.close()
}, { flush: 'post' })
</script>

<template>
  <dialog
    ref="dialog"
    class="ui-dialog"
    :aria-label="label"
    @close="open = false"
  >
    <slot />
  </dialog>
</template>
