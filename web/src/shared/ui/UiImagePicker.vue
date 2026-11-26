<script setup lang="ts">
import { useTemplateRef } from 'vue'

import { imageFileAccept } from '../imageFiles'
import UiIconButton from './UiIconButton.vue'

withDefaults(defineProps<{ label: string; disabled?: boolean; multiple?: boolean }>(), { multiple: true })
const emit = defineEmits<{ files: [files: File[]] }>()
const input = useTemplateRef('input')

function selected(event: Event) {
  if (!(event.target instanceof HTMLInputElement)) return
  emit('files', Array.from(event.target.files ?? []))
  event.target.value = ''
}
</script>

<template>
  <input
    ref="input"
    type="file"
    :accept="imageFileAccept"
    :multiple="multiple"
    hidden
    @change="selected"
  >
  <UiIconButton
    icon="image-circle-line"
    :label="label"
    :disabled="disabled"
    @click="input?.click()"
  />
</template>
