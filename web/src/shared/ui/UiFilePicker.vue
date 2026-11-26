<script setup lang="ts">
import { useTemplateRef } from 'vue'

import { UiButton } from './elements'

defineProps<{ label: string; disabled?: boolean }>()
const emit = defineEmits<{ select: [file: File] }>()
const input = useTemplateRef('input')

function selected(event: Event) {
  if (!(event.target instanceof HTMLInputElement)) return
  const file = event.target.files?.[0]
  if (file) emit('select', file)
  event.target.value = ''
}
</script>

<template>
  <input
    ref="input"
    type="file"
    hidden
    :disabled="disabled"
    @change="selected"
  >
  <UiButton class="secondary-button" :disabled="disabled" @click="input?.click()">{{ label }}</UiButton>
</template>
