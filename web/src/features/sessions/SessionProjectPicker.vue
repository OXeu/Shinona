<script setup lang="ts">
import { computed, onUnmounted, ref, useId } from 'vue'
import { useRouter } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiDropdown from '../../shared/ui/UiDropdown.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import UiMoreMenu from '../../shared/ui/UiMoreMenu.vue'
import { UiBox, UiButton, UiForm, UiInline, UiSubheading } from '../../shared/ui/elements'
import { allProjects, defaultProject, projectScopeKey, projectTargetId } from './projectScope'
import type { ProjectScope } from './projectScope'
import type { Project } from './sessionApi'
import { useSessionStore } from './sessionStore'

const props = defineProps<{ hideAll?: boolean; compact?: boolean }>()
const store = useSessionStore()
const router = useRouter()
const inputId = useId()
const request = new AbortController()
const dialogOpen = ref(false)
const action = ref<'add' | 'rename' | 'remove'>('add')
const target = ref<Project>()
const name = ref('')
const saving = ref(false)

const choices = computed(() => [
  { scope: allProjects, name: t('projects.all'), icon: 'folder-open-line', project: undefined },
  { scope: defaultProject, name: t('projects.default'), icon: 'folder-3-line', project: undefined },
  ...store.projects.map((project) => ({
    scope: { kind: 'project', id: project.id } as const, name: project.name, icon: 'folder-3-line', project,
  })),
].filter((item) => !props.hideAll || item.scope.kind !== 'all')
  .map((item) => ({ ...item, key: projectScopeKey(item.scope) })))
const selected = computed(() => props.hideAll ? store.draftProject : store.selectedProject)
const selectedKey = computed(() => projectScopeKey(selected.value))
const label = computed(() => choices.value.find((item) => item.key === selectedKey.value)?.name ??
  t(props.hideAll ? 'projects.default' : 'projects.all'))
const dialogTitle = computed(() => t(action.value === 'add' ? 'projects.add' : action.value === 'rename' ? 'projects.rename' : 'projects.remove'))

function openDialog(next: typeof action.value, project?: Project) {
  action.value = next
  target.value = project
  name.value = project?.name ?? ''
  dialogOpen.value = true
}

function manageSandbox(project: ProjectScope) {
  if (project.kind !== 'all') {
    void router.push({ name: 'project-sandbox', params: { projectId: projectTargetId(project) ?? '' } })
  }
}

function choose(scope: ProjectScope, close: (restoreFocus?: boolean) => void) {
  if (props.hideAll) {
    if (scope.kind !== 'all') store.selectDraftProject(scope)
  } else {
    store.selectProject(scope)
  }
  close(true)
}

async function save() {
  if (saving.value) return
  const item = target.value
  const operation = action.value
  const { signal } = request
  saving.value = true
  try {
    let saved = false
    if (operation === 'add') saved = await store.createProject(name.value, signal, !props.hideAll)
    else if (item && operation === 'rename') saved = await store.renameProject(item.id, name.value, signal)
    else if (item) saved = await store.removeProject(item.id, signal)
    if (!saved) return
    dialogOpen.value = false
    notify.success(t(operation === 'add' ? 'projects.added' : operation === 'rename' ? 'projects.renamed' : 'projects.removed'))
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, operation === 'add' ? 'projects.addFailed' : 'projects.updateFailed'))
  } finally {
    saving.value = false
  }
}

onUnmounted(() => request.abort())
</script>

<template>
  <UiDropdown
    class="project-dropdown"
    :class="{ 'project-dropdown--compact': compact }"
    :label="t('common.choiceSummary', { label: t('workspace.project'), value: label })"
    scroll
    floating
  >
    <template #trigger>
      <UiBox class="project-trigger">
        <UiIcon :name="selected.kind === 'all' ? 'folder-open-line' : 'folder-3-line'" />
        <UiInline class="project-name">{{ label }}</UiInline>
        <UiIcon name="arrow-down-s-line" />
      </UiBox>
    </template>
    <template #default="{ close }">
      <UiBox class="project-options">
        <UiBox v-for="item in choices" :key="item.key" class="project-option">
          <UiButton
            class="menu-item"
            :aria-current="item.key === selectedKey ? 'true' : undefined"
            :title="item.name"
            @click="choose(item.scope, close)"
          >
            <UiIcon :name="item.icon" /> <UiInline class="project-option-name">{{ item.name }}</UiInline>
            <UiIcon class="menu-tail" name="check-line" :hidden="item.key !== selectedKey" />
          </UiButton>
          <UiMoreMenu
            :hidden="item.scope.kind === 'all'"
            :label="t('projects.actionsFor', { name: item.name })"
            boundary=".project-options"
            :disabled="saving"
          >
            <template #default="{ close: closeActions }">
              <UiButton
                class="menu-item"
                @click="closeActions(); close(); manageSandbox(item.scope)"
              >
                <UiIcon name="terminal-box-line" /> {{ t('sandbox.manage') }}
              </UiButton>
              <UiButton
                class="menu-item"
                :hidden="!item.project"
                @click="closeActions(); close(true); openDialog('rename', item.project)"
              >
                <UiIcon name="edit-line" /> {{ t('projects.rename') }}
              </UiButton>
              <UiButton
                class="menu-item danger"
                :hidden="!item.project"
                @click="closeActions(); close(true); openDialog('remove', item.project)"
              >
                <UiIcon name="delete-bin-line" /> {{ t('projects.remove') }}
              </UiButton>
            </template>
          </UiMoreMenu>
        </UiBox>
      </UiBox>
      <UiButton class="menu-item" @click="close(true); openDialog('add')">
        <UiIcon name="add-line" /> {{ t('projects.add') }}
      </UiButton>
    </template>
  </UiDropdown>

  <UiDialog v-model="dialogOpen" :label="dialogTitle">
    <UiSubheading>{{ dialogTitle }}</UiSubheading>
    <UiForm @submit.prevent="save">
      <UiBox :hidden="action === 'remove'">
        <UiInput
          :id="inputId"
          v-model="name"
          :label="t('projects.name')"
          autofocus
        />
      </UiBox>
      <UiBox class="dialog-message" :hidden="action !== 'remove'">
        {{ t('projects.confirmRemove', { name: target?.name ?? '' }) }}
      </UiBox>
      <UiBox class="form-actions">
        <UiButton class="secondary-button" :disabled="saving" @click="dialogOpen = false">{{ t('common.cancel') }}</UiButton>
        <UiButton type="submit" :disabled="saving">{{ action === 'rename' ? t('projects.saveName') : dialogTitle }}</UiButton>
      </UiBox>
    </UiForm>
  </UiDialog>
</template>

<style lang="less" scoped>
.project-dropdown { flex-shrink: 0; }
.project-dropdown :deep(.dropdown-trigger) { border-radius: var(--radius-full); }
.project-dropdown :deep(.dropdown-content) { text-align: start; }

.project-trigger {
  display: flex;
  height: 32px;
  max-width: 17rem;
  align-items: center;
  gap: 4px;
  padding-inline: 8px;
  border: 1px solid var(--border);
  border-radius: var(--radius-full);
  color: var(--muted);
  background: var(--background);
  font-size: 0.875rem;
}

.project-dropdown--compact .project-trigger { max-width: 7.5rem; }

.project-trigger:hover {
  color: var(--foreground);
  background: var(--sidebar-bg);
}

.project-dropdown[open] .project-trigger {
  color: var(--foreground);
  background: var(--selection-bg);
}

.project-name {
  min-width: 0;
  overflow: hidden;
  font-size: 0.8rem;
  white-space: nowrap;
  text-overflow: ellipsis;
}

.project-option { display: flex; align-items: center; }
.project-option > .menu-item { flex: 1 1 auto; min-width: 0; }
.project-option > .more-menu { flex: 0 0 auto; }
.project-option > .more-menu :deep(.more-menu-trigger) { background: transparent; }
.project-option-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
