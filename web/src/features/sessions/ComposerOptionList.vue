<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../../shared/i18n'
import { UiBox, UiButton, UiInline } from '../../shared/ui/elements'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiSearchField from '../../shared/ui/UiSearchField.vue'

const props = withDefaults(defineProps<{
  label: string
  options: { value: string; label: string }[]
  selected: string
  emptyLabel?: string
  searchable?: boolean
  showLabel?: boolean
}>(), { emptyLabel: '' })
const emit = defineEmits<{ select: [value: string] }>()
const query = defineModel<string>('query', { required: true })
const filtered = computed(() => {
  const term = props.searchable ? query.value.trim().toLocaleLowerCase() : ''
  return term ? props.options.filter((option) => option.label.toLocaleLowerCase().includes(term)) : props.options
})
</script>

<template>
  <UiBox role="group" :aria-label="label" class="config-option-list">
    <UiInline class="config-option-label" :hidden="!showLabel">{{ label }}</UiInline>
    <UiBox :hidden="!searchable || !options.length">
      <UiSearchField v-model="query" :label="t('composer.filterModels')" @keydown.enter.prevent />
    </UiBox>
    <UiBox class="config-option-rows">
      <UiInline class="config-option-empty" :hidden="!!filtered.length">
        {{ emptyLabel && !options.length ? emptyLabel : t('composer.noMatchingModels') }}
      </UiInline>
      <UiButton
        v-for="option in filtered"
        :key="option.value"
        class="config-option"
        :title="option.label"
        :aria-pressed="option.value === selected"
        @click="emit('select', option.value)"
      >
        <UiInline class="config-option-text">{{ option.label }}</UiInline>
        <UiIcon :hidden="option.value !== selected" name="check-line" />
      </UiButton>
    </UiBox>
  </UiBox>
</template>

<style lang="less" scoped>
.config-option-list { display: grid; min-width: 0; gap: 6px; }
.config-option-label {
  padding-inline: 4px;
  color: var(--muted);
  font-size: 0.6875rem;
  font-weight: 600;
}
.config-option-rows {
  display: grid;
  gap: 2px;
  max-height: min(14rem, 37dvh);
  overflow: auto;
  padding: 2px;
  scrollbar-width: thin;
}
.config-option {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
  min-height: 32px;
  min-width: 0;
  padding: 5px 8px;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--foreground);
  font-size: 0.75rem;
  font-weight: 500;
  text-align: start;
}
.config-option:hover { background: var(--sidebar-bg); }
.config-option[aria-pressed='true'] { background: var(--selection-bg); font-weight: 600; }
.config-option .ui-icon { font-size: 0.875rem; }
.config-option-text { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.config-option-empty { padding: 5px 8px; color: var(--muted); font-size: 0.75rem; }
@media (max-width: 767px) {
  .config-option { min-height: 44px; }
}
</style>
