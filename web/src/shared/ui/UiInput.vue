<script setup lang="ts">
defineOptions({ inheritAttrs: false })
defineProps<{ id: string; label: string }>()
const value = defineModel<string>({ required: true })
</script>

<template>
  <div class="field">
    <div :class="{ 'field-heading': $slots.help }">
      <label :for="id">{{ label }}</label>
      <slot name="help" />
    </div>
    <input
      :id="id"
      v-model="value"
      class="input"
      v-bind="$attrs"
    >
  </div>
</template>
