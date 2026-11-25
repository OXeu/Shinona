<script setup lang="ts">
import { useTemplateRef } from 'vue'
import UiIconButton from './UiIconButton.vue'

defineProps<{ label: string; disabled?: boolean }>()
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
    accept="image/png,image/jpeg,image/webp"
    multiple
    hidden
    @change="selected"
  >
  <UiIconButton
    icon="image-line"
    :label="label"
    :disabled="disabled"
    @click="input?.click()"
  />
</template>
