<script setup lang="ts">
defineProps<{ label: string; indeterminate?: boolean; disabled?: boolean }>()
const value = defineModel<boolean>({ required: true })
</script>

<template>
  <label class="ui-checkbox">
    <input
      v-model="value"
      type="checkbox"
      :aria-label="label"
      :indeterminate.prop="!!indeterminate"
      :disabled="disabled"
    >
    <slot />
  </label>
</template>

<style lang="less" scoped>
.ui-checkbox { display: inline-flex; align-items: center; gap: 6px; }
.ui-checkbox input {
  width: 15px;
  height: 15px;
  margin: 0;
  accent-color: var(--primary);
}
</style>
