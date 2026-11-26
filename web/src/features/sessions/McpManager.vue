<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { navigateToOAuth } from '../../shared/http'
import { t } from '../../shared/i18n'
import type { MessageKey } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiChoiceDropdown from '../../shared/ui/UiChoiceDropdown.vue'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton, UiForm, UiInline, UiList, UiListItem, UiSection, UiSubheading } from '../../shared/ui/elements'
import { mcpSettingsApi } from './mcpSettingsApi'
import type { McpServer, McpSettings } from './mcpSettingsApi'

const settings = defineModel<McpSettings | null>({ required: true })
const props = defineProps<{ disabled: boolean }>()
const open = ref(false)
const action = ref<'edit' | 'remove' | 'reset'>('edit')
const target = ref<McpServer | null>(null)
const draftName = ref('')
const draftUrl = ref('')
const draftAuth = ref('none')
const draftToken = ref('')
const busy = ref(false)
const request = new AbortController()
const authOptions = computed(() => [
  { value: 'none', label: t('mcp.noAuth') },
  { value: 'bearer', label: t('mcp.bearer') },
  { value: 'oauth', label: t('mcp.oauth') },
])
const retainingToken = computed(() => target.value?.hasToken && target.value.url === draftUrl.value.trim())
const dialogTitle = computed(() => {
  switch (action.value) {
    case 'edit':
      return t(target.value ? 'mcp.edit' : 'mcp.add')
    case 'remove':
      return t('mcp.removePrompt', { name: target.value?.name ?? '' })
    case 'reset':
      return t('mcp.resetPrompt')
  }
})

function edit(server: McpServer | null) {
  target.value = server
  action.value = 'edit'
  draftName.value = server?.name ?? ''
  draftUrl.value = server?.url ?? ''
  draftAuth.value = server?.oauth ? 'oauth' : server?.hasToken ? 'bearer' : 'none'
  draftToken.value = ''
  open.value = true
}

function confirmRemove(server: McpServer) {
  target.value = server
  action.value = 'remove'
  open.value = true
}

function confirmReset() {
  target.value = null
  action.value = 'reset'
  open.value = true
}

async function change(update: () => Promise<McpSettings>, success: MessageKey) {
  if (busy.value || props.disabled) return
  busy.value = true
  try {
    settings.value = await update()
    open.value = false
    if (!request.signal.aborted) notify.success(t(success))
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, 'mcp.updateFailed'))
  } finally {
    busy.value = false
  }
}

function save() {
  const name = draftName.value.trim()
  const url = draftUrl.value.trim()
  if (!name || !url) {
    notify.error(t('composer.invalidMcp'))
    return
  }
  const token = draftToken.value.trim()
  if (draftAuth.value === 'bearer' && !token && !retainingToken.value) {
    notify.error(t(target.value?.hasToken ? 'mcp.tokenForChangedUrl' : 'mcp.tokenRequired'))
    return
  }
  const input = {
    previousName: target.value?.name ?? null,
    name,
    url,
    token: draftAuth.value === 'bearer' ? token || null : '',
    oauth: draftAuth.value === 'oauth',
  }
  return change(() => mcpSettingsApi.save(input, request.signal), 'mcp.saved')
}

function removeOrReset() {
  if (action.value === 'reset') {
    return change(() => mcpSettingsApi.reset(request.signal), 'mcp.reset')
  }
  const name = target.value?.name
  if (action.value === 'remove' && name) {
    return change(() => mcpSettingsApi.remove(name, request.signal), 'mcp.removed')
  }
}

async function toggleConnection(server: McpServer) {
  if (busy.value || props.disabled) return
  busy.value = true
  try {
    if (server.connected) {
      await mcpSettingsApi.disconnect(server.name, request.signal)
      if (request.signal.aborted) return
      server.connected = false
      notify.success(t('mcp.oauthDisconnected'))
    } else {
      const { url } = await mcpSettingsApi.connect(server.name, request.signal)
      if (!request.signal.aborted) navigateToOAuth(url)
    }
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, 'mcp.oauthFailed'))
  } finally {
    busy.value = false
  }
}

watch(open, (visible) => {
  if (!visible) {
    draftToken.value = ''
  }
})
onUnmounted(() => request.abort())
</script>

<template>
  <UiSection :aria-label="t('composer.mcp')" class="mcp-manager">
    <UiBox class="mcp-actions" :hidden="!settings">
      <UiButton :disabled="disabled || busy" @click="edit(null)">{{ t('mcp.add') }}</UiButton>
      <UiButton
        :hidden="!settings?.overridden"
        :disabled="disabled || busy"
        class="secondary-button"
        @click="confirmReset"
      >
        {{ t('mcp.removeAll') }}
      </UiButton>
    </UiBox>
    <UiBox :hidden="!settings || !!settings.servers.length" class="mcp-empty">{{ t('mcp.empty') }}</UiBox>
    <UiList class="mcp-list" :hidden="!settings?.servers.length">
      <UiListItem v-for="server in settings?.servers ?? []" :key="server.name" class="mcp-row">
        <UiBox class="mcp-detail">
          <UiInline class="mcp-name">{{ server.name }}</UiInline>
          <UiInline>{{ server.url }}</UiInline>
          <UiInline>{{ t(server.oauth ? (server.connected ? 'mcp.oauthConnectedLabel' : 'mcp.oauth') : server.hasToken ? 'mcp.bearer' : 'mcp.noAuth') }}</UiInline>
        </UiBox>
        <UiBox class="mcp-actions">
          <UiButton
            class="secondary-button"
            :hidden="!server.oauth"
            :aria-label="t(server.connected ? 'mcp.disconnectNamed' : 'mcp.connectNamed', { name: server.name })"
            :disabled="disabled || busy"
            @click="toggleConnection(server)"
          >
            {{ t(server.connected ? 'mcp.disconnect' : 'mcp.connect') }}
          </UiButton>
          <UiButton
            class="secondary-button"
            :aria-label="t('mcp.editNamed', { name: server.name })"
            :disabled="disabled || busy"
            @click="edit(server)"
          >
            {{ t('mcp.edit') }}
          </UiButton>
          <UiButton
            class="secondary-button"
            :aria-label="t('mcp.removeNamed', { name: server.name })"
            :disabled="disabled || busy"
            @click="confirmRemove(server)"
          >
            {{ t('mcp.remove') }}
          </UiButton>
        </UiBox>
      </UiListItem>
    </UiList>

    <UiDialog v-model="open" :label="dialogTitle">
      <UiSubheading>{{ dialogTitle }}</UiSubheading>
      <UiForm :hidden="action !== 'edit'" @submit.prevent="save">
        <UiInput
          id="mcp-server-name"
          v-model="draftName"
          :label="t('mcp.name')"
          :disabled="busy"
        />
        <UiInput
          id="mcp-server-url"
          v-model="draftUrl"
          :label="t('mcp.url')"
          :disabled="busy"
        />
        <UiChoiceDropdown
          v-model="draftAuth"
          :label="t('mcp.auth')"
          :options="authOptions"
          :disabled="busy"
        />
        <UiBox :hidden="draftAuth !== 'bearer'">
          <UiInput
            id="mcp-server-token"
            v-model="draftToken"
            :label="t('mcp.bearer')"
            :placeholder="retainingToken ? t('mcp.keepToken') : undefined"
            :disabled="busy"
            type="password"
            autocomplete="new-password"
          />
        </UiBox>
        <UiBox class="form-actions">
          <UiButton class="secondary-button" :disabled="busy" @click="open = false">{{ t('mcp.cancel') }}</UiButton>
          <UiButton type="submit" :disabled="disabled || busy">{{ t('mcp.save') }}</UiButton>
        </UiBox>
      </UiForm>
      <UiForm :hidden="action === 'edit'" @submit.prevent="removeOrReset">
        <UiBox class="form-actions">
          <UiButton class="secondary-button" :disabled="busy" @click="open = false">{{ t('mcp.cancel') }}</UiButton>
          <UiButton type="submit" :disabled="disabled || busy">
            {{ t(action === 'reset' ? 'mcp.removeAll' : 'mcp.remove') }}
          </UiButton>
        </UiBox>
      </UiForm>
    </UiDialog>
  </UiSection>
</template>

<style lang="less" scoped>
.mcp-list { list-style: none; margin: 12px 0 0; padding: 0; }
.mcp-row {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  padding-block: 10px;
  border-top: 1px solid var(--border);
}
.mcp-actions { display: flex; flex-wrap: wrap; gap: 4px; }
.mcp-empty {
  display: grid;
  place-items: center;
  block-size: 9rem;
  margin-block-start: 12px;
  padding: 12px;
  color: var(--muted);
  text-align: center;
}
.mcp-detail { display: grid; gap: 2px; min-width: 0; color: var(--muted); overflow-wrap: anywhere; }
.mcp-name { color: var(--foreground); font-weight: 600; }
</style>
