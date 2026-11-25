<script setup lang="ts">
import UiIcon from './UiIcon.vue'

defineProps<{ icon: string; label: string }>()
</script>

<template>
  <button
    type="button"
    class="icon-button"
    :aria-label="label"
    :title="label"
  >
    <UiIcon :name="icon" />
  </button>
</template>
