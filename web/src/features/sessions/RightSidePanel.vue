<script setup lang="ts">
import { computed, defineAsyncComponent, ref, useTemplateRef, watch } from 'vue'

import { t } from '../../shared/i18n'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiButton, UiSection } from '../../shared/ui/elements'
import WorkspaceRepository from './WorkspaceRepository.vue'
import { useWorkbench } from './workbench'
import type { WorkspaceView } from './workbench'

defineProps<{ sessionId?: string; projectId?: string | null; active: boolean; runningTurnId?: string }>()
const emit = defineEmits<{ unauthorized: [] }>()
const TerminalPanel = defineAsyncComponent(() => import('./TerminalPanel.vue'))
const { workspaceId, workspaceOpen, terminalOpen, view, repositoryView, selectView, leaveTerminal } = useWorkbench()
const repository = useTemplateRef('repository')
const terminalMounted = ref(false)
const tabs = computed<{ id: WorkspaceView; label: string }[]>(() => [
  { id: 'changes', label: t('repository.changes') },
  { id: 'files', label: t('repository.files') },
  { id: 'history', label: t('repository.history') },
  { id: 'terminal', label: t('terminal.heading') },
])

watch(terminalOpen, (open) => {
  if (open) terminalMounted.value = true
}, { immediate: true })

function terminalEmpty() {
  terminalMounted.value = false
  leaveTerminal()
}
</script>

<template>
  <UiSection :id="workspaceId" class="right-panel" :aria-label="t('workspace.panel')">
    <UiBox class="workspace-switcher">
      <UiBox class="workspace-tabs" role="group" :aria-label="t('workspace.views')">
        <UiButton
          v-for="item in tabs"
          :key="item.id"
          class="workspace-tab"
          :aria-pressed="view === item.id"
          :disabled="!sessionId"
          @click="selectView(item.id)"
        >
          {{ item.label }}
        </UiButton>
      </UiBox>
      <UiIconButton
        v-if="view !== 'terminal' && !repository?.inspection"
        icon="refresh-line"
        :label="t('repository.refresh')"
        :disabled="!sessionId || repository?.refreshDisabled"
        @click="repository?.refresh()"
      />
    </UiBox>

    <UiBox v-if="!sessionId" class="workspace-empty">{{ t('repository.selectSession') }}</UiBox>
    <WorkspaceRepository
      v-if="sessionId"
      v-show="view !== 'terminal'"
      :key="projectId ?? ''"
      ref="repository"
      :session-id="sessionId"
      :active="active"
      :running-turn-id="runningTurnId"
      :view="repositoryView"
      :visible="workspaceOpen && view !== 'terminal'"
      @unauthorized="emit('unauthorized')"
    />
    <TerminalPanel
      v-if="terminalMounted && sessionId"
      v-show="view === 'terminal'"
      class="workspace-terminal"
      :session-id="sessionId"
      :visible="terminalOpen"
      @empty="terminalEmpty"
      @unauthorized="emit('unauthorized')"
    />
  </UiSection>
</template>

<style src="./styles/workspace-panel.less" lang="less"></style>
