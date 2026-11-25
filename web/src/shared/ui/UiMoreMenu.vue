<script setup lang="ts">
import UiDropdown from './UiDropdown.vue'
import UiIcon from './UiIcon.vue'
import { UiBox } from './elements'

defineProps<{ label: string; boundary: string; disabled?: boolean }>()
</script>

<template>
  <UiDropdown
    class="more-menu"
    :label="label"
    :boundary="boundary"
    :disabled="disabled"
    floating
  >
    <template #trigger>
      <UiBox class="icon-button more-menu-trigger"><UiIcon name="more-line" /></UiBox>
    </template>
    <template #default="{ close }">
      <slot :close="close" />
    </template>
  </UiDropdown>
</template>

<style lang="less" scoped>
.more-menu-trigger { width: 28px; min-width: 28px; height: 28px; }
</style>
