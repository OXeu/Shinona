<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { t } from '../../shared/i18n'
import UiCheckbox from '../../shared/ui/UiCheckbox.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiSplitPanel from '../../shared/ui/UiSplitPanel.vue'
import UiTextarea from '../../shared/ui/UiTextarea.vue'
import { UiBox, UiButton, UiForm, UiInline, UiList, UiListItem, UiSection } from '../../shared/ui/elements'
import type { WorkspaceChanges } from './workspaceApi'

const props = defineProps<{ changes: WorkspaceChanges | null; busy: boolean; commitBlocked: boolean }>()
const emit = defineEmits<{
  preview: [path: string, status: string]
  commit: [message: string, paths: string[]]
}>()
const selected = ref<string[]>([])
const message = ref('')
const listSize = ref(70)
const entries = computed(() => props.changes?.entries ?? [])
const allSelected = computed(() => !!entries.value.length && selected.value.length === entries.value.length)

watch(() => props.changes, (changes, previous) => {
  const before = previous?.selection
  if (!changes?.selection || !before || changes.selection.root !== before.root || changes.selection.projectId !== before.projectId) {
    selected.value = []
    return
  }
  const current = new Set(changes.entries.map(({ path }) => path))
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
  <UiSection class="workspace-view workspace-changes" :aria-label="t('repository.changedFiles')">
    <UiBox class="workspace-controls">
      <UiCheckbox
        :label="t('repository.selectAll')"
        :model-value="allSelected"
        :indeterminate="!!selected.length && !allSelected"
        :disabled="!entries.length || busy"
        @update:model-value="selected = $event ? entries.map(({ path }) => path) : []"
      />
      <UiIcon v-if="changes?.branch" name="git-branch-line" />
      <UiInline class="workspace-ellipsis" :title="changes?.branch">
        {{ changes?.branch || t('repository.changedFiles') }}
      </UiInline>
      <UiInline class="workspace-meta workspace-line-counts">
        <UiInline class="workspace-additions">+{{ changes?.additions ?? 0 }}</UiInline>
        <UiInline class="workspace-deletions">-{{ changes?.deletions ?? 0 }}</UiInline>
      </UiInline>
    </UiBox>
    <UiSplitPanel
      v-model="listSize"
      class="workspace-changes-split"
      direction="column"
      :start-min="52"
      :end-min="132"
      :label="t('repository.resizeChanges')"
    >
      <template #start>
        <UiBox class="workspace-scroll">
          <UiInline v-if="changes && !entries.length" class="workspace-note">{{ t('repository.noChanges') }}</UiInline>
          <UiInline v-if="changes?.truncated" class="workspace-note">{{ t('repository.truncated') }}</UiInline>
          <UiList class="workspace-rows">
            <UiListItem v-for="entry in entries" :key="entry.path" class="workspace-change">
              <UiCheckbox
                :label="t('repository.selectPath', { path: entry.path })"
                :model-value="selected.includes(entry.path)"
                :disabled="busy"
                @update:model-value="setChecked(entry.path, $event)"
              />
              <UiButton
                class="workspace-row"
                :title="entry.path"
                :disabled="busy"
                @click="emit('preview', entry.path, entry.status)"
              >
                <UiIcon name="file-text-line" />
                <UiInline class="workspace-ellipsis">{{ entry.path }}</UiInline>
                <UiInline class="workspace-status" :data-status="entry.status">{{ entry.status }}</UiInline>
              </UiButton>
            </UiListItem>
          </UiList>
        </UiBox>
      </template>
      <template #end>
        <UiForm class="workspace-commit ui-form--custom" @submit.prevent="emit('commit', message, selected)">
          <UiTextarea
            id="workspace-commit-message"
            v-model="message"
            :label="t('repository.commitMessage')"
            :disabled="busy"
            maxlength="4096"
            rows="2"
          />
          <UiBox class="workspace-commit-actions">
            <UiInline class="workspace-meta">{{ t('repository.selected', { count: selected.length, total: entries.length }) }}</UiInline>
            <UiButton type="submit" :disabled="busy || commitBlocked || !selected.length || !message.trim()">
              {{ t('repository.commit') }}
            </UiButton>
          </UiBox>
        </UiForm>
      </template>
    </UiSplitPanel>
  </UiSection>
</template>
