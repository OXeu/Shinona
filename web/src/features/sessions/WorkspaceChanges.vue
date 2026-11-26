<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { t } from '../../shared/i18n'
import UiCheckbox from '../../shared/ui/UiCheckbox.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiSplitPanel from '../../shared/ui/UiSplitPanel.vue'
import UiTextarea from '../../shared/ui/UiTextarea.vue'
import { UiBox, UiButton, UiForm, UiHeader, UiInline, UiList, UiListItem, UiSection, UiSubheading } from '../../shared/ui/elements'
import type { WorkspaceChanges } from './workspaceApi'

const props = defineProps<{ changes: WorkspaceChanges | null; busy: boolean; turnActive: boolean }>()
const emit = defineEmits<{ preview: [path: string]; commit: [message: string, paths: string[]] }>()
const selected = ref<string[]>([])
const message = ref('')
const split = ref(60)
const entries = computed(() => props.changes?.entries ?? [])
const allSelected = computed(() => !!entries.value.length && selected.value.length === entries.value.length)

watch(entries, (items) => {
  const current = new Set(items.map(({ path }) => path))
  selected.value = selected.value.filter((path) => current.has(path))
})

function setChecked(path: string, checked: boolean) {
  selected.value = checked ? [...selected.value, path] : selected.value.filter((item) => item !== path)
}

defineExpose({
  resetCommit() {
    message.value = ''
    selected.value = []
  },
})
</script>

<template>
  <UiSplitPanel
    v-model="split"
    direction="column"
    :start-min="100"
    :end-min="138"
    :label="t('repository.resizeCommit')"
  >
    <template #start>
      <UiSection class="repo-card" :aria-label="t('repository.changedFiles')">
        <UiHeader class="repo-card-header">
          <UiCheckbox
            :label="t('repository.selectAll')"
            :model-value="allSelected"
            :indeterminate="!!selected.length && !allSelected"
            :disabled="!entries.length || busy"
            @update:model-value="selected = $event ? entries.map(({ path }) => path) : []"
          />
          <UiSubheading>{{ t('repository.changedFiles') }}</UiSubheading>
          <UiInline class="repo-pill" :hidden="!changes?.branch">{{ changes?.branch }}</UiInline>
          <UiInline class="repo-pill">{{ entries.length }}</UiInline>
        </UiHeader>
        <UiBox class="repo-card-scroll">
          <UiInline class="repo-note" :hidden="!!entries.length">{{ t('repository.noChanges') }}</UiInline>
          <UiInline class="repo-note" :hidden="!changes?.truncated">{{ t('repository.truncated') }}</UiInline>
          <UiList class="repo-rows">
            <UiListItem v-for="entry in entries" :key="entry.path" class="repo-change">
              <UiCheckbox
                :label="t('repository.selectPath', { path: entry.path })"
                :model-value="selected.includes(entry.path)"
                :disabled="busy"
                @update:model-value="setChecked(entry.path, $event)"
              />
              <UiButton
                class="repo-row"
                :title="entry.path"
                :disabled="busy"
                @click="emit('preview', entry.path)"
              >
                <UiIcon name="file-text-line" />
                <UiInline class="repo-row-name">{{ entry.path }}</UiInline>
                <UiInline class="repo-status" :data-status="entry.status">{{ entry.status }}</UiInline>
              </UiButton>
            </UiListItem>
          </UiList>
        </UiBox>
      </UiSection>
    </template>
    <template #end>
      <UiForm class="repo-card repo-commit ui-form--custom" @submit.prevent="emit('commit', message, selected)">
        <UiHeader class="repo-card-header">
          <UiIcon name="git-commit-line" />
          <UiSubheading>{{ t('repository.commit') }}</UiSubheading>
          <UiInline class="repo-pill">{{ t('repository.selected', { count: selected.length, total: entries.length }) }}</UiInline>
        </UiHeader>
        <UiTextarea
          id="workspace-commit-message"
          v-model="message"
          :label="t('repository.commitMessage')"
          :disabled="busy || turnActive"
          maxlength="4096"
          rows="3"
        />
        <UiBox class="repo-commit-actions">
          <UiButton type="submit" :disabled="busy || turnActive || !selected.length || !message.trim()">
            {{ t('repository.commit') }}
          </UiButton>
        </UiBox>
      </UiForm>
    </template>
  </UiSplitPanel>
</template>
