<script setup lang="ts">
const props = defineProps<{ id: string; label: string; placeholder: string; disabled?: boolean; maxBytes: number }>()
const value = defineModel<string>({ required: true })
const emit = defineEmits<{ tooLarge: [] }>()
const mask = 'secret-entered'

function paste(event: ClipboardEvent) {
  const text = event.clipboardData?.getData('text/plain') ?? ''
  if (!text) return
  if (new TextEncoder().encode(text).length > props.maxBytes) {
    emit('tooLarge')
    return
  }
  value.value = text
}

function restore(event: Event) {
  if (event.target instanceof HTMLInputElement) event.target.value = value.value ? mask : ''
}
</script>

<template>
  <div class="field">
    <div :class="{ 'field-heading': $slots.help }">
      <label :for="id">{{ label }}</label>
      <slot name="help" />
    </div>
    <input
      :id="id"
      class="input"
      type="password"
      :value="value ? mask : ''"
      :placeholder="placeholder"
      :disabled="disabled"
      autocomplete="new-password"
      @beforeinput.prevent
      @input="restore"
      @paste.prevent="paste"
      @keydown.backspace.prevent="value = ''"
      @keydown.delete.prevent="value = ''"
    >
  </div>
</template>
