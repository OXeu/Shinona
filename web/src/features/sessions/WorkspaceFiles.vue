<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../../shared/i18n'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiButton, UiInline, UiList, UiListItem, UiSection } from '../../shared/ui/elements'
import type { WorkspaceEntry, WorkspaceList } from './workspaceApi'

const props = defineProps<{ files: WorkspaceList<WorkspaceEntry> | null; folder: string; root: string; busy: boolean }>()
const emit = defineEmits<{ browse: [path: string]; preview: [path: string]; setRoot: [] }>()
const parentFolder = computed(() => {
  const separator = props.folder.lastIndexOf('/')
  return separator < 0 ? '.' : props.folder.slice(0, separator)
})
const rootLabel = computed(() => t('repository.currentRoot', { path: props.root || t('workspace.name') }))
const folderName = computed(() => props.folder === '.' ? t('workspace.name') : props.folder)

function openEntry(entry: WorkspaceEntry) {
  if (entry.directory) emit('browse', entry.path)
  else emit('preview', entry.path)
}
</script>

<template>
  <UiSection class="workspace-view" :aria-label="t('repository.files')">
    <UiBox class="workspace-controls">
      <UiIconButton
        icon="arrow-up-line"
        :label="t('repository.parentFolder')"
        :disabled="folder === '.'"
        @click="emit('browse', parentFolder)"
      />
      <UiInline class="workspace-ellipsis" :title="folderName">{{ folderName }}</UiInline>
      <UiButton
        v-if="folder !== (root || '.')"
        class="workspace-set-root secondary-button"
        :disabled="busy"
        :title="t('repository.setRootHint')"
        @click="emit('setRoot')"
      >
        {{ t('repository.setRoot') }}
      </UiButton>
    </UiBox>
    <UiBox class="workspace-scroll">
      <UiInline v-if="files && !files.entries.length" class="workspace-note">{{ t('repository.noFiles') }}</UiInline>
      <UiInline v-if="files?.truncated" class="workspace-note">{{ t('repository.truncated') }}</UiInline>
      <UiList class="workspace-rows">
        <UiListItem v-for="entry in files?.entries" :key="entry.path">
          <UiButton class="workspace-row" :title="entry.name" @click="openEntry(entry)">
            <UiIcon :name="entry.directory ? 'folder-3-line' : 'file-text-line'" />
            <UiInline class="workspace-ellipsis">{{ entry.name }}</UiInline>
            <UiIcon v-if="entry.directory" name="arrow-right-s-line" />
          </UiButton>
        </UiListItem>
      </UiList>
    </UiBox>
  </UiSection>
</template>
