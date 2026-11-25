<script setup lang="ts">
import { t } from '../../shared/i18n'
import { UiHeader, UiHeading } from '../../shared/ui/elements'
import UiIconButton from '../../shared/ui/UiIconButton.vue'

defineProps<{
  title: string
  panelOpen: boolean
  compact: boolean
  workspaceAvailable: boolean
  terminalOpen: boolean
  creating: boolean
}>()
const emit = defineEmits<{ sessions: []; create: []; togglePanel: []; toggleTerminal: [] }>()
</script>

<template>
  <UiHeader class="agent-header">
    <UiIconButton
      icon="list-check-2"
      :label="t('sessions.show')"
      :hidden="!compact"
      @click="emit('sessions')"
    />
    <UiHeading class="agent-heading">{{ title }}</UiHeading>
    <UiIconButton
      icon="terminal-line"
      :label="t(terminalOpen ? 'terminal.close' : 'terminal.open')"
      :aria-expanded="terminalOpen"
      :hidden="!workspaceAvailable"
      @click="emit('toggleTerminal')"
    />
    <UiIconButton
      icon="add-line"
      :label="t('sessions.new')"
      :disabled="creating"
      @click="emit('create')"
    />
    <UiIconButton
      icon="layout-right-line"
      :label="t(panelOpen ? 'workspace.hidePanel' : 'workspace.showPanel')"
      :aria-expanded="panelOpen"
      :hidden="!workspaceAvailable"
      @click="emit('togglePanel')"
    />
  </UiHeader>
</template>
