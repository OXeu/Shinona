<script setup lang="ts">
import { useTemplateRef } from 'vue'

import UiIcon from './UiIcon.vue'

defineProps<{ label: string }>()
const model = defineModel<string>({ required: true })
const input = useTemplateRef('input')

defineExpose({ focus: () => input.value?.focus({ preventScroll: true }) })
</script>

<template>
  <label class="input search-field" :title="label">
    <UiIcon name="search-line" />
    <input
      ref="input"
      v-model="model"
      type="search"
      :aria-label="label"
      :placeholder="label"
    >
  </label>
</template>
