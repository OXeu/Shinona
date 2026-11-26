<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiChoiceDropdown from '../../shared/ui/UiChoiceDropdown.vue'
import UiLanguagePicker from '../../shared/ui/UiLanguagePicker.vue'
import { UiBox, UiButton, UiHeading, UiNav, UiSection } from '../../shared/ui/elements'
import { runningAction, setRunningAction } from './composerPreferences'
import InstanceSettings from './InstanceSettings.vue'
import SSHServerSettings from './SSHServerSettings.vue'
import type { McpSettings } from './mcpSettingsApi'
import { settingsApi } from './settingsApi'
import type { SettingField, SSHServer } from './settingsApi'

type Section = 'preferences' | 'model' | 'sandbox' | 'ssh' | 'mcp' | 'oauth' | 'mail'

const savingInstance = ref(false)
const fields = ref<SettingField[]>([])
const modelFields = ref<SettingField[]>([])
const sshServers = ref<SSHServer[]>([])
const mcp = ref<McpSettings | null>(null)
const request = new AbortController()
const route = useRoute()
const router = useRouter()
const active = ref<Section>(route.query.section === 'ssh' ? 'ssh' : 'preferences')
const sections = computed(() => {
  const administrator = fields.value.length > 0
  const entries: { id: Section; label: string; visible: boolean }[] = [
    { id: 'preferences', label: t('settings.preferences'), visible: true },
    { id: 'model', label: t('settings.model'), visible: modelFields.value.length > 0 },
    { id: 'sandbox', label: t('workspace.sandbox'), visible: administrator },
    { id: 'ssh', label: t('ssh.servers'), visible: true },
    { id: 'mcp', label: t('composer.mcp'), visible: mcp.value !== null },
    { id: 'oauth', label: t('auth.oauth'), visible: administrator },
    { id: 'mail', label: t('settings.mail'), visible: administrator },
  ]
  return entries.filter(({ visible }) => visible)
})
const runningActions = computed(() => [
  { value: 'queue', label: t('composer.queue') },
  { value: 'steer', label: t('composer.steer') },
])
async function load() {
  const response = await settingsApi.load(request.signal)
  fields.value = response.applicationSettings
  modelFields.value = response.modelSettings
  mcp.value = response.mcpSettings
  sshServers.value = response.sshServers
}

onMounted(async () => {
  try {
    await load()
    const { mcpOAuth, ...query } = route.query
    if (!request.signal.aborted && (mcpOAuth === 'connected' || mcpOAuth === 'failed')) {
      if (mcp.value) active.value = 'mcp'
      if (mcpOAuth === 'connected') notify.success(t('mcp.oauthConnected'))
      else notify.error(t('mcp.oauthFailed'))
      await router.replace({ query })
    }
  } catch {
    if (!request.signal.aborted) notify.error(t('settings.loadFailed'))
  }
})
onUnmounted(() => request.abort())

async function saveInstance(changes: { key: string; value: string | null }[]) {
  if (savingInstance.value || !changes.length) return
  savingInstance.value = true
  try {
    if (active.value === 'model') {
      const { updateModelSettings } = await settingsApi.saveModel(changes, request.signal)
      modelFields.value = updateModelSettings
    } else {
      const { updateApplicationSettings } = await settingsApi.save(changes, request.signal)
      fields.value = updateApplicationSettings
    }
    if (!request.signal.aborted) notify.success(t('settings.saved'))
    try {
      await load()
    } catch {
      if (!request.signal.aborted) notify.error(t('settings.refreshFailed'))
    }
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, 'settings.saveFailed'))
  } finally {
    savingInstance.value = false
  }
}
</script>

<template>
  <UiBox class="page">
    <UiHeading class="settings-heading">{{ t('nav.settings') }}</UiHeading>
    <UiNav class="settings-nav" :aria-label="t('settings.sections')">
      <UiButton
        v-for="section in sections"
        :key="section.id"
        class="settings-tab"
        :aria-pressed="active === section.id"
        @click="active = section.id"
      >
        {{ section.label }}
      </UiButton>
    </UiNav>
    <UiSection class="preferences" :hidden="active !== 'preferences'" :aria-label="t('settings.preferences')">
      <UiLanguagePicker />
      <UiChoiceDropdown
        :label="t('composer.runningDefault')"
        :model-value="runningAction"
        :options="runningActions"
        @update:model-value="setRunningAction"
      />
    </UiSection>
    <SSHServerSettings v-model="sshServers" :active="active === 'ssh'" />
    <InstanceSettings
      :key="active === 'model' ? 'model' : 'instance'"
      v-model:mcp="mcp"
      :section="active"
      :fields="active === 'model' ? modelFields : fields"
      :saving="savingInstance"
      @save="saveInstance"
    />
  </UiBox>
</template>

<style lang="less" scoped>
.settings-heading { margin-bottom: 1rem; }
.settings-nav {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  padding-bottom: 8px;
  margin-bottom: 16px;
  border-bottom: 1px solid var(--border);
}
.settings-tab {
  padding: 6px 10px;
  color: var(--muted);
  background: transparent;
}
.settings-tab:hover { color: var(--foreground); background: var(--sidebar-bg); }
.settings-tab[aria-pressed='true'] { color: var(--foreground); background: var(--selection-bg); }
.preferences:not([hidden]) { display: grid; gap: 16px; }
.preferences :deep(.field) { margin: 0; }
</style>
