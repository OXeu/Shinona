<script setup lang="ts">
import { computed, defineAsyncComponent, nextTick, onMounted, onUnmounted, ref, useTemplateRef, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { RetryBackoff } from '../../shared/retryBackoff'
import { UiBox } from '../../shared/ui/elements'
import WorkspaceChangesPanel from './WorkspaceChanges.vue'
import WorkspaceFiles from './WorkspaceFiles.vue'
import WorkspaceHistory from './WorkspaceHistory.vue'
import type { RepositoryView } from './workbench'
import { workspaceApi } from './workspaceApi'
import type { GitCommit, WorkspaceChanges, WorkspaceEntry, WorkspaceList } from './workspaceApi'
import type { WorkspaceInspection } from './workspaceInspection'

const WorkspaceInspectionPanel = defineAsyncComponent(() => import('./WorkspaceInspection.vue'))

const props = defineProps<{
  sessionId: string
  active: boolean
  runningTurnId?: string
  view: RepositoryView
  visible: boolean
}>()
const emit = defineEmits<{ unauthorized: [] }>()
const changesPanel = useTemplateRef('changesPanel')
const changes = ref<WorkspaceChanges | null>(null)
const history = ref<WorkspaceList<GitCommit> | null>(null)
const files = ref<WorkspaceList<WorkspaceEntry> | null>(null)
const folder = ref('.')
const root = ref('')
const loading = ref(false)
const mutating = ref(false)
const loadMessage = ref('')
const inspection = ref<WorkspaceInspection>()
const backoff = new RetryBackoff()
let reading: AbortController | undefined
let writing: AbortController | undefined
let retry: ReturnType<typeof setTimeout> | undefined
let inspectionTrigger: HTMLElement | undefined

const lists = { changes, files, history }
const state = computed(() => {
  if (lists[props.view].value) return ''
  return loadMessage.value || (loading.value ? t('repository.loading') : '')
})
const refreshDisabled = computed(() => loading.value || mutating.value)

function pause() {
  clearTimeout(retry)
  reading?.abort()
  loading.value = false
}

async function load() {
  pause()
  if (!props.visible || document.hidden || mutating.value) return

  loadMessage.value = ''
  reading = new AbortController()
  const { signal } = reading
  loading.value = true
  try {
    await loadView(signal)
    if (!signal.aborted) backoff.reset()
  } catch (error) {
    if (signal.aborted || unauthorized(error)) return
    if (error instanceof HttpError &&
      (error.code === 'WORKSPACE_BUSY' || (props.active && error.code === 'WORKSPACE_UNAVAILABLE'))) {
      const delay = backoff.next()
      if (delay !== undefined && !document.hidden) {
        loadMessage.value = t('repository.starting')
        retry = setTimeout(load, delay)
        return
      }
    }
    loadMessage.value = messageFor(error, 'repository.loadFailed')
  } finally {
    if (!signal.aborted) loading.value = false
  }
}

async function loadView(signal: AbortSignal) {
  const session = props.sessionId
  switch (props.view) {
    case 'changes': {
      const result = await workspaceApi.changes(session, signal)
      if (!signal.aborted) changes.value = result
      break
    }
    case 'files': {
      const [savedRoot, result] = await Promise.all([
        workspaceApi.root(session, signal),
        workspaceApi.directory(session, folder.value, signal),
      ])
      if (!signal.aborted) {
        root.value = savedRoot
        files.value = result
      }
      break
    }
    case 'history': {
      const result = await workspaceApi.history(session, signal)
      if (!signal.aborted) history.value = result
      break
    }
  }
}

function unauthorized(error: unknown) {
  if (!(error instanceof HttpError) || error.status !== 401) return false
  emit('unauthorized')
  return true
}

function restart() {
  backoff.reset()
  return load()
}

function browse(path: string) {
  if (path === folder.value) return
  files.value = null
  folder.value = path
}

function inspect(target: WorkspaceInspection) {
  if (mutating.value) return
  inspectionTrigger = document.activeElement instanceof HTMLElement ? document.activeElement : undefined
  inspection.value = target
}

function inspectChange(path: string, status: string) {
  inspect({ title: path, path, status, diff: true })
}

function inspectCommit(commit: GitCommit) {
  inspect({ title: commit.short + ' · ' + commit.subject, path: '', diff: true, revision: commit.hash })
}

function inspectFile(path: string) {
  inspect({ title: path, path, diff: false })
}

async function closeInspection(restoreFocus = false) {
  const trigger = inspectionTrigger
  inspection.value = undefined
  inspectionTrigger = undefined
  if (restoreFocus) {
    await nextTick()
    trigger?.focus({ preventScroll: true })
  }
}

async function mutate(action: (signal: AbortSignal) => Promise<void>, failure: 'repository.rootFailed' | 'repository.commitFailed') {
  pause()
  writing = new AbortController()
  const { signal } = writing
  mutating.value = true
  try {
    await action(signal)
  } catch (error) {
    if (signal.aborted || unauthorized(error)) return
    if (error instanceof HttpError && error.code === 'WORKSPACE_SELECTION_CHANGED') changes.value = null
    notify.error(messageFor(error, failure))
  } finally {
    if (!signal.aborted) {
      mutating.value = false
      void restart()
    }
  }
}

function setRoot() {
  if (loading.value || mutating.value || folder.value === (root.value || '.')) return
  const target = folder.value === '.' ? '' : folder.value
  return mutate(async (signal) => {
    const result = await workspaceApi.setRoot(props.sessionId, target, signal)
    if (signal.aborted) return
    root.value = result
    changes.value = null
    history.value = null
    changesPanel.value?.resetCommit()
    notify.success(t('repository.rootSet', { path: result || t('workspace.name') }))
  }, 'repository.rootFailed')
}

function commit(message: string, paths: string[]) {
  const selection = changes.value?.selection
  if (!selection || props.active || mutating.value || loading.value || !paths.length || !message.trim()) return
  return mutate(async (signal) => {
    await workspaceApi.commit(props.sessionId, message, paths, selection, signal)
    if (signal.aborted) return
    changesPanel.value?.resetCommit()
    changes.value = null
    history.value = null
    notify.success(t('repository.committed'))
  }, 'repository.commitFailed')
}

function resume() {
  if (document.hidden) {
    pause()
  } else if (props.visible && !loading.value && !mutating.value) {
    void restart()
  }
}

watch([() => props.view, () => props.active, () => props.runningTurnId, () => props.visible, folder], restart, { immediate: true })

defineExpose({ inspection, closeInspection, refresh: restart, refreshDisabled })

onMounted(() => {
  document.addEventListener('visibilitychange', resume)
  window.addEventListener('focus', resume)
  window.addEventListener('online', resume)
})
onUnmounted(() => {
  document.removeEventListener('visibilitychange', resume)
  window.removeEventListener('focus', resume)
  window.removeEventListener('online', resume)
  pause()
  writing?.abort()
})
</script>

<template>
  <UiBox class="workspace-content">
    <WorkspaceInspectionPanel
      v-if="inspection"
      :session-id="sessionId"
      :inspection="inspection"
      @back="closeInspection(true)"
      @unauthorized="emit('unauthorized')"
    />
    <UiBox v-else-if="state" class="workspace-empty">{{ state }}</UiBox>
    <UiBox v-else-if="loadMessage" class="workspace-note">{{ loadMessage }}</UiBox>
    <WorkspaceChangesPanel
      v-show="view === 'changes' && !inspection && !state"
      ref="changesPanel"
      :changes="changes"
      :busy="mutating"
      :commit-blocked="active || loading || !changes?.selection"
      @preview="inspectChange"
      @commit="commit"
    />
    <WorkspaceFiles
      v-show="view === 'files' && !inspection && !state"
      :files="files"
      :folder="folder"
      :root="root"
      :busy="loading || mutating"
      @browse="browse"
      @preview="inspectFile"
      @set-root="setRoot"
    />
    <WorkspaceHistory
      v-show="view === 'history' && !inspection && !state"
      :history="history"
      @preview="inspectCommit"
    />
  </UiBox>
</template>
