<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../i18n'
import { UiBox, UiButton, UiInline } from './elements'
import UiDropdown from './UiDropdown.vue'
import UiIcon from './UiIcon.vue'

const props = defineProps<{
  label: string
  options: { value: string; label: string; lang?: string }[]
  disabled?: boolean
  placeholder?: string
  side?: 'top' | 'bottom'
  boundary?: string
  textTrigger?: boolean
}>()
const value = defineModel<string>({ required: true })
const selected = computed(() => props.options.find((option) => option.value === value.value))
const displayValue = computed(() => selected.value?.label ?? props.placeholder ?? t('common.unavailable'))
</script>

<template>
  <UiBox :class="textTrigger ? 'choice-text' : 'field'">
    <UiInline class="field-label" :hidden="textTrigger">{{ label }}</UiInline>
    <UiDropdown
      class="choice-dropdown"
      :label="t('common.choiceSummary', { label, value: displayValue })"
      :disabled="disabled || !options.length"
      align="start"
      :boundary="boundary"
      :side="side"
      scroll
    >
      <template #trigger>
        <UiBox class="choice-trigger">
          <UiInline :lang="selected?.lang">{{ displayValue }}</UiInline>
          <UiIcon name="arrow-down-s-line" />
        </UiBox>
      </template>
      <template #default="{ close }">
        <UiButton
          v-for="option in options"
          :key="option.value"
          :lang="option.lang"
          class="menu-item"
          :aria-pressed="option.value === value"
          @click="value = option.value; close(true)"
        >
          {{ option.label }}
          <UiIcon
            class="menu-tail"
            name="check-line"
            :hidden="option.value !== value"
          />
        </UiButton>
      </template>
    </UiDropdown>
  </UiBox>
</template>

<style lang="less" scoped>
.choice-dropdown { display: block; }
.choice-trigger {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--surface);
}
.choice-dropdown:hover .choice-trigger { border-color: var(--border-hover); }
.choice-text .choice-dropdown { display: inline-block; }
.choice-text .choice-trigger {
  gap: 4px;
  padding: 2px 0;
  border: 0;
  color: var(--muted);
  background: transparent;
}
.choice-text .choice-dropdown:hover .choice-trigger { color: var(--foreground); }
</style>
