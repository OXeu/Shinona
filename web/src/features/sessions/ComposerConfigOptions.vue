<script setup lang="ts">
import { computed, nextTick, ref, useId, watch } from 'vue'

import { t } from '../../shared/i18n'
import { UiBox, UiButton, UiInline, UiSubheading } from '../../shared/ui/elements'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiDropdown from '../../shared/ui/UiDropdown.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { useMediaQuery } from '../../shared/ui/useMediaQuery'
import ComposerDefaultActions from './ComposerDefaultActions.vue'
import ComposerOptionList from './ComposerOptionList.vue'
import type { ComposerDefaults, ComposerOptions } from './sessionApi'

const props = defineProps<{ models: ComposerOptions['models']; defaults?: ComposerDefaults | null; savingDefaults: boolean }>()
const emit = defineEmits<{ saveDefaults: [options: ComposerDefaults]; reset: [] }>()
const model = defineModel<string>({ required: true })
const permission = defineModel<string>('permission', { required: true })
const thinking = defineModel<string>('thinking', { required: true })

type Category = 'model' | 'permission' | 'thinking'

interface OptionCategory {
  label: string
  selected: string
  options: { value: string; label: string }[]
  emptyLabel?: string
  searchable?: boolean
}

const selection = { model, permission, thinking }
const defaultSaved = computed(() => props.defaults?.model === model.value && props.defaults.permission === permission.value &&
  props.defaults.thinking === thinking.value)
const groupId = useId()
const mobile = useMediaQuery('(max-width: 767px)')
const desktopOpen = ref(false)
const sheetOpen = ref(false)
const active = ref<Category | null>(null)
const query = ref('')
const categories = computed<Record<Category, OptionCategory>>(() => ({
  model: {
    label: t('settings.model'),
    selected: model.value,
    options: props.models.map(({ id }) => ({ value: id, label: id })),
    emptyLabel: t('composer.noModelOptions'),
    searchable: true,
  },
  permission: {
    label: t('composer.permission'),
    selected: permission.value,
    options: [
      { value: 'WORKSPACE', label: t('composer.workspaceAccess') },
      { value: 'READ_ONLY', label: t('composer.readOnly') },
    ],
  },
  thinking: {
    label: t('composer.thinking'),
    selected: thinking.value,
    options: [
      { value: 'off', label: t('composer.off') },
      { value: 'minimal', label: t('composer.minimal') },
      { value: 'low', label: t('composer.low') },
      { value: 'medium', label: t('composer.medium') },
      { value: 'high', label: t('composer.high') },
      { value: 'xhigh', label: t('composer.extraHigh') },
    ],
  },
}))
const configSummary = computed(() => Object.values(categories.value)
  .map((category) => t('common.choiceSummary', { label: category.label, value: currentLabel(category) }))
  .join(' · '))

function currentLabel(category: OptionCategory) {
  return category.options.find(({ value }) => value === category.selected)?.label
    ?? (category.selected || t('common.unavailable'))
}

function choose(category: Category, value: string) {
  selection[category].value = value
}

function saveDefaults() {
  emit('saveDefaults', { model: model.value, permission: permission.value, thinking: thinking.value })
}

function onOpenChange(open: boolean) {
  desktopOpen.value = open
  if (!open) {
    active.value = null
    query.value = ''
  }
}

async function focusOptions(category: Category) {
  active.value = category
  await nextTick()
  const panel = document.getElementById(`${groupId}-${category}`)
  const group = categories.value[category]
  const firstControl = group.searchable && group.options.length
    ? panel?.querySelector('input')
    : panel?.querySelector<HTMLButtonElement>('button[aria-pressed="true"]') ?? panel?.querySelector('button')
  firstControl?.focus()
}

watch(mobile, (narrow) => {
  if (!narrow) sheetOpen.value = false
})
watch(sheetOpen, (open) => {
  if (!open) query.value = ''
})
</script>

<template>
  <UiBox class="composer-config">
    <UiDropdown
      :hidden="mobile"
      :disabled="mobile"
      :label="t('common.choiceSummary', { label: t('composer.options'), value: configSummary })"
      boundary=".composer-toolbar"
      align="start"
      side="top"
      bare
      @open-change="onOpenChange"
    >
      <template #trigger>
        <UiBox class="config-trigger" :data-open="desktopOpen">
          <UiIcon name="brain-line" />
          <UiInline class="config-trigger-name">{{ model || t('composer.options') }}</UiInline>
          <UiInline class="config-trigger-permission">· {{ currentLabel(categories.permission) }}</UiInline>
          <UiIcon :name="desktopOpen ? 'arrow-up-s-line' : 'arrow-down-s-line'" />
        </UiBox>
      </template>
      <UiBox class="config-options-menu" role="group" :aria-label="t('composer.options')">
        <ComposerDefaultActions
          class="config-categories"
          :ready="defaults !== undefined"
          :saved="defaultSaved"
          :saving="savingDefaults"
          @save="saveDefaults"
          @reset="emit('reset')"
        >
          <UiButton
            v-for="(category, key) in categories"
            :key="key"
            class="config-category"
            :aria-controls="`${groupId}-${key}`"
            :aria-expanded="active === key"
            @pointerenter="active = key"
            @focus="active = key"
            @click="focusOptions(key)"
            @keydown.right.prevent="focusOptions(key)"
          >
            <UiInline class="config-category-label">{{ category.label }}</UiInline>
            <UiInline class="config-category-value">{{ currentLabel(category) }}</UiInline>
            <UiIcon name="arrow-right-s-line" />
          </UiButton>
        </ComposerDefaultActions>
        <UiBox
          v-for="(category, key) in categories"
          :id="`${groupId}-${key}`"
          :key="key"
          class="config-choices"
          :hidden="active !== key"
        >
          <ComposerOptionList
            v-model:query="query"
            :label="category.label"
            :options="category.options"
            :selected="category.selected"
            :empty-label="category.emptyLabel"
            :searchable="category.searchable"
            @select="choose(key, $event)"
          />
        </UiBox>
      </UiBox>
    </UiDropdown>

    <UiIconButton
      :hidden="!mobile"
      class="config-mobile-trigger"
      icon="brain-line"
      :label="t('common.choiceSummary', { label: t('composer.options'), value: configSummary })"
      aria-haspopup="dialog"
      :aria-expanded="sheetOpen"
      @click="sheetOpen = true"
    />
    <UiDialog v-model="sheetOpen" :label="t('composer.options')" class="config-sheet">
      <UiBox class="config-sheet-header">
        <UiSubheading>{{ t('composer.options') }}</UiSubheading>
        <UiIconButton icon="close-line" :label="t('composer.closeOptions')" @click="sheetOpen = false" />
      </UiBox>
      <ComposerDefaultActions
        :ready="defaults !== undefined"
        :saved="defaultSaved"
        :saving="savingDefaults"
        @save="saveDefaults"
        @reset="emit('reset')"
      >
        <UiBox class="config-sheet-grid">
          <ComposerOptionList
            v-for="(category, key) in categories"
            :key="key"
            v-model:query="query"
            :class="{ 'config-sheet-models': key === 'model' }"
            :label="category.label"
            :options="category.options"
            :selected="category.selected"
            :empty-label="category.emptyLabel"
            :searchable="category.searchable"
            show-label
            @select="choose(key, $event)"
          />
        </UiBox>
      </ComposerDefaultActions>
    </UiDialog>
  </UiBox>
</template>

<style src="./styles/composer-config-options.less" lang="less" scoped></style>
