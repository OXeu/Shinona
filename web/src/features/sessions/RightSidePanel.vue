<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, useTemplateRef, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { languageTag, t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { RetryBackoff } from '../../shared/retryBackoff'
import { formatTime } from '../../shared/time'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import UiSearchField from '../../shared/ui/UiSearchField.vue'
import { UiBox, UiButton, UiHeader, UiInline, UiList, UiListItem, UiPre, UiSection, UiSubheading, UiTime } from '../../shared/ui/elements'
import WorkspaceChangesPanel from './WorkspaceChanges.vue'
import { workspaceApi } from './workspaceApi'
import type { GitCommit, WorkspaceChanges, WorkspaceEntry, WorkspaceList, WorkspacePreview } from './workspaceApi'

const props = defineProps<{ sessionId?: string; projectId?: string | null; active: boolean; runningTurnId?: string; visible: boolean }>()
const emit = defineEmits<{ unauthorized: [] }>()
const tab = ref<'changes' | 'history' | 'files'>('changes')
const tabs = computed(() => [
  { id: 'changes' as const, label: t('repository.changes'), icon: 'git-branch-line' },
  { id: 'history' as const, label: t('repository.history'), icon: 'history-line' },
  { id: 'files' as const, label: t('repository.files'), icon: 'folder-3-line' },
])
const changesPanel = useTemplateRef('changesPanel')
const changes = ref<WorkspaceChanges | null>(null)
const history = ref<WorkspaceList<GitCommit> | null>(null)
const files = ref<WorkspaceList<WorkspaceEntry> | null>(null)
const folder = ref('.')
const root = ref('')
const settingRoot = ref(false)
const filter = ref('')
const loading = ref(false)
const mutating = ref(false)
const loadMessage = ref('')
const previewOpen = ref(false)
const previewTitle = ref('')
const previewDiff = ref(false)
const preview = ref<WorkspacePreview | null>(null)
const previewError = ref('')
const previewLoading = ref(false)
let reading: AbortController | undefined
let retry: ReturnType<typeof setTimeout> | undefined
const backoff = new RetryBackoff()
let writing: AbortController | undefined
let settingRootRequest: AbortController | undefined
let readingPreview: AbortController | undefined
const visibleCommits = computed(() => {
  const query = filter.value.trim().toLocaleLowerCase(languageTag())
  return (history.value?.entries ?? []).filter((item) =>
    [item.subject, item.author, item.hash].some((value) => value.toLocaleLowerCase(languageTag()).includes(query)))
})
const previewLines = computed(() => preview.value?.text.split('\n') ?? [])
const parentFolder = computed(() => folder.value.includes('/') ? folder.value.slice(0, folder.value.lastIndexOf('/')) : '.')
// The files panel doubles as the picker, so the button acts on the open folder.
const currentRoot = computed(() => root.value === '' ? '.' : root.value)
const rootLabel = computed(() => root.value === '' ? t('workspace.name') : root.value)
// Hovering the path reveals the working directory that scopes the Git panels.
const folderTitle = computed(() => root.value === '' ? folder.value : `${root.value} · ${t('repository.currentRoot', { path: rootLabel.value })}`)
const state = computed(() => {
  if (!props.sessionId) return t('repository.selectSession')
  if (loading.value) return t('repository.loading')
  return loadMessage.value
})

function selectTab(id: typeof tab.value) {
  tab.value = id
}

async function load() {
  clearTimeout(retry)
  reading?.abort()
  loading.value = false
  loadMessage.value = ''
  const session = props.sessionId
  if (!session || !props.visible || document.hidden) return

  reading = new AbortController()
  const { signal } = reading
  loading.value = true
  try {
    await Promise.all([
      workspaceApi.root(session, signal).then((savedRoot) => {
        if (!signal.aborted) root.value = savedRoot
      }),
      loadTab(session, signal),
    ])
    if (!signal.aborted) backoff.reset()
  } catch (error) {
    if (signal.aborted) return
    if (error instanceof HttpError && error.status === 401) {
      emit('unauthorized')
      return
    }
    if (error instanceof HttpError &&
      (error.code === 'WORKSPACE_BUSY' || (props.active && error.code === 'WORKSPACE_UNAVAILABLE'))) {
      const delay = backoff.next()
      if (delay !== undefined && !document.hidden) {
        loadMessage.value = t('repository.starting')
        retry = setTimeout(() => {
          void load()
        }, delay)
        return
      }
    }
    loadMessage.value = messageFor(error, 'repository.loadFailed')
  } finally {
    if (!signal.aborted) loading.value = false
  }
}

function restart() {
  backoff.reset()
  return load()
}

async function loadTab(session: string, signal: AbortSignal) {
  switch (tab.value) {
    case 'changes': {
      const result = await workspaceApi.changes(session, signal)
      if (!signal.aborted) changes.value = result
      break
    }
    case 'history': {
      const result = await workspaceApi.history(session, signal)
      if (!signal.aborted) history.value = result
      break
    }
    case 'files': {
      const result = await workspaceApi.directory(session, folder.value, signal)
      if (!signal.aborted) files.value = result
      break
    }
  }
}

async function openPreview(title: string, path: string, diff: boolean, revision?: string) {
  const session = props.sessionId
  if (!session || loading.value || mutating.value) return
  readingPreview?.abort()
  readingPreview = new AbortController()
  const { signal } = readingPreview
  previewTitle.value = title
  previewDiff.value = diff
  preview.value = null
  previewError.value = ''
  previewLoading.value = true
  previewOpen.value = true
  try {
    const result = await workspaceApi.preview(session, path, diff, revision, signal)
    if (!signal.aborted) preview.value = result
  } catch (error) {
    if (!signal.aborted) previewError.value = messageFor(error, 'repository.previewFailed')
  } finally {
    if (!signal.aborted) previewLoading.value = false
  }
}

function openEntry(entry: WorkspaceEntry) {
  if (entry.directory) folder.value = entry.path
  else void openPreview(entry.path, entry.path, false)
}

async function setAsWorkspace() {
  const session = props.sessionId
  if (!session || folder.value === currentRoot.value || mutating.value || settingRoot.value) return
  const target = folder.value === '.' ? '' : folder.value
  settingRootRequest?.abort()
  settingRootRequest = new AbortController()
  const { signal } = settingRootRequest
  settingRoot.value = true
  try {
    const result = await workspaceApi.setRoot(session, target, signal)
    if (signal.aborted) return
    root.value = result
    notify.success(t('repository.rootSet', { path: rootLabel.value }))
    await restart()
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'repository.rootFailed'))
  } finally {
    if (!signal.aborted) settingRoot.value = false
  }
}

async function commit(message: string, paths: string[]) {
  const session = props.sessionId
  if (!session || props.active || mutating.value || loading.value || !paths.length || !message.trim()) return
  writing = new AbortController()
  const { signal } = writing
  mutating.value = true
  try {
    await workspaceApi.commit(session, message, paths, signal)
    if (signal.aborted) return
    changesPanel.value?.resetCommit()
    notify.success(t('repository.committed'))
    await restart()
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'repository.commitFailed'))
  } finally {
    if (!signal.aborted) mutating.value = false
  }
}

watch([() => props.sessionId, () => props.projectId], () => {
  writing?.abort()
  settingRootRequest?.abort()
  settingRoot.value = false
  readingPreview?.abort()
  previewOpen.value = false
  mutating.value = false
  changes.value = null
  history.value = null
  files.value = null
  folder.value = '.'
  root.value = ''
  filter.value = ''
})
watch([() => props.sessionId, () => props.projectId, () => props.active, () => props.runningTurnId, () => props.visible, tab, folder], restart, { immediate: true })
watch(previewOpen, (value) => {
  if (!value) readingPreview?.abort()
})
function resumeWorkspace() {
  if (document.hidden) {
    clearTimeout(retry)
    reading?.abort()
    loading.value = false
    return
  }
  if (props.visible && !loading.value && !mutating.value && !settingRoot.value) void restart()
}

onMounted(() => {
  document.addEventListener('visibilitychange', resumeWorkspace)
  window.addEventListener('focus', resumeWorkspace)
  window.addEventListener('online', resumeWorkspace)
})
onUnmounted(() => {
  document.removeEventListener('visibilitychange', resumeWorkspace)
  window.removeEventListener('focus', resumeWorkspace)
  window.removeEventListener('online', resumeWorkspace)
  clearTimeout(retry)
  reading?.abort()
  writing?.abort()
  settingRootRequest?.abort()
  readingPreview?.abort()
})
</script>

<template>
  <UiSection class="right-panel" :aria-label="t('workspace.panel')">
    <UiHeader class="repo-toolbar">
      <UiBox class="repo-tabs" :aria-label="t('workspace.views')">
        <UiButton
          v-for="item in tabs"
          :key="item.id"
          class="repo-tab"
          :aria-pressed="tab === item.id"
          :disabled="mutating || settingRoot"
          @click="selectTab(item.id)"
        >
          <UiIcon :name="item.icon" /> {{ item.label }}
        </UiButton>
      </UiBox>
      <UiIconButton
        icon="refresh-line"
        :label="t('repository.refresh')"
        :disabled="!sessionId || loading || mutating"
        @click="restart"
      />
    </UiHeader>

    <UiBox class="repo-panes">
      <UiBox class="repo-empty" :hidden="!state">
        <UiInline>{{ state }}</UiInline>
      </UiBox>
      <WorkspaceChangesPanel
        :key="sessionId"
        ref="changesPanel"
        :hidden="tab !== 'changes' || !!state"
        :changes="changes"
        :busy="mutating"
        :turn-active="active"
        @preview="(path) => openPreview(path, path, true)"
        @commit="commit"
      />
      <UiSection class="repo-card" :aria-label="t('repository.history')" :hidden="tab !== 'history' || !!state">
        <UiBox class="repo-tools">
          <UiSearchField v-model="filter" :label="t('repository.searchHistory')" />
        </UiBox>
        <UiBox class="repo-card-scroll">
          <UiInline class="repo-note" :hidden="!!visibleCommits.length">{{ t(filter.trim() ? 'repository.noMatches' : 'repository.noCommits') }}</UiInline>
          <UiInline class="repo-note" :hidden="!history?.truncated">{{ t('repository.truncated') }}</UiInline>
          <UiList class="repo-rows">
            <UiListItem v-for="item in visibleCommits" :key="item.hash">
              <UiButton class="repo-row repo-commit-row" @click="openPreview(item.short + ' · ' + item.subject, '', true, item.hash)">
                <UiIcon name="git-commit-line" />
                <UiBox class="repo-commit-description">
                  <UiInline class="repo-row-name">{{ item.subject }}</UiInline>
                  <UiInline class="repo-meta">
                    {{ item.short }} · {{ item.author }} · <UiTime :datetime="item.date">{{ formatTime(item.date) }}</UiTime>
                  </UiInline>
                </UiBox>
              </UiButton>
            </UiListItem>
          </UiList>
        </UiBox>
      </UiSection>
      <UiSection class="repo-card" :aria-label="t('repository.files')" :hidden="tab !== 'files' || !!state">
        <UiHeader class="repo-card-header">
          <UiIconButton
            icon="arrow-up-line"
            :label="t('repository.parentFolder')"
            :disabled="folder === '.'"
            @click="folder = parentFolder"
          />
          <UiInline class="repo-path" :title="folderTitle">{{ folder === '.' ? t('workspace.name') : folder }}</UiInline>
          <UiButton
            class="repo-set-root"
            :hidden="folder === currentRoot"
            :disabled="mutating || settingRoot"
            :title="t('repository.setRootHint')"
            @click="setAsWorkspace"
          >
            {{ t('repository.setRoot') }}
          </UiButton>
        </UiHeader>
        <UiBox class="repo-card-scroll">
          <UiInline class="repo-note" :hidden="!!files?.entries.length">{{ t('repository.noFiles') }}</UiInline>
          <UiInline class="repo-note" :hidden="!files?.truncated">{{ t('repository.truncated') }}</UiInline>
          <UiList class="repo-rows">
            <UiListItem v-for="entry in files?.entries" :key="entry.path">
              <UiButton class="repo-row" :title="entry.name" @click="openEntry(entry)">
                <UiIcon :name="entry.directory ? 'folder-3-line' : 'file-text-line'" />
                <UiInline class="repo-row-name">{{ entry.name }}</UiInline>
                <UiIcon name="arrow-right-s-line" :hidden="!entry.directory" />
              </UiButton>
            </UiListItem>
          </UiList>
        </UiBox>
      </UiSection>
    </UiBox>

    <UiDialog v-model="previewOpen" class="repo-preview" :label="previewTitle">
      <UiHeader class="repo-card-header">
        <UiSubheading>{{ previewTitle }}</UiSubheading>
        <UiIconButton icon="close-line" :label="t('repository.closePreview')" @click="previewOpen = false" />
      </UiHeader>
      <UiInline class="repo-note" :hidden="!previewLoading">{{ t('repository.loading') }}</UiInline>
      <UiInline class="repo-note" :hidden="!previewError">{{ previewError }}</UiInline>
      <UiInline class="repo-note" :hidden="!preview?.truncated">{{ t('repository.truncated') }}</UiInline>
      <UiPre class="repo-preview-text" :hidden="!preview || previewDiff">{{ preview?.text }}</UiPre>
      <UiPre class="repo-preview-text" :hidden="!preview || !previewDiff">
        <UiInline
          v-for="(line, index) in previewDiff ? previewLines : []"
          :key="index"
          class="repo-diff-line"
          :data-marker="line[0]"
        >
          {{ line }}
        </UiInline>
      </UiPre>
    </UiDialog>
  </UiSection>
</template>

<style src="./styles/workspace-panel.less" lang="less"></style>
