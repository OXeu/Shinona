<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import type { MessageKey } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiChoiceDropdown from '../../shared/ui/UiChoiceDropdown.vue'
import UiDisclosure from '../../shared/ui/UiDisclosure.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import { UiBox, UiButton, UiForm, UiSection, UiSubheading } from '../../shared/ui/elements'
import InstanceSettingField from './InstanceSettingField.vue'
import McpManager from './McpManager.vue'
import type { McpSettings } from './mcpSettingsApi'
import { settingsApi } from './settingsApi'
import type { SettingField } from './settingsApi'

const props = defineProps<{ fields: SettingField[]; saving: boolean; section: string }>()
const mcp = defineModel<McpSettings | null>('mcp', { required: true })
const emit = defineEmits<{ save: [changes: { key: string; value: string | null }[]] }>()

const groups: Partial<Record<string, { label: MessageKey; fields: Record<string, MessageKey> }>> = {
  sandbox: { label: 'workspace.sandbox', fields: {} },
  model: { label: 'settings.model', fields: {
    modelBaseUrl: 'composer.baseUrl', modelApiKey: 'settings.modelKey',
  } },
  mcp: { label: 'composer.mcp', fields: {} },
  oauth: { label: 'auth.oauth', fields: {
    oauthIssuer: 'settings.issuer', oauthClientId: 'settings.clientId', oauthClientSecret: 'settings.clientSecret',
  } },
  mail: { label: 'settings.mail', fields: {
    mailFrom: 'settings.mailFrom', mailApiKey: 'settings.mailApiKey',
  } },
}
const e2bFields: Record<string, MessageKey> = {
  e2bApiKey: 'settings.e2bKey', e2bTemplate: 'settings.e2bTemplate', e2bBaseUrl: 'settings.e2bBaseUrl',
}
const connectionFields: Record<string, MessageKey> = {
  e2bProxyUrl: 'settings.e2bProxy', e2bDomain: 'settings.e2bDomain',
}
const group = computed(() => groups[props.section])
const drafts = reactive<Record<string, string | null>>({})

function labeledFields(labels: Record<string, MessageKey>) {
  return Object.entries(labels).flatMap(([key, label]) => {
    const field = props.fields.find((item) => item.key === key)
    return field ? [{ field, label }] : []
  })
}
const displayedFields = computed(() => labeledFields(group.value?.fields ?? {}))
const e2b = computed(() => props.section === 'sandbox' ? labeledFields(e2bFields) : [])
const connection = computed(() => props.section === 'sandbox' ? labeledFields(connectionFields) : [])
const modelsField = computed(() => props.fields.find((field) => field.key === 'modelId'))
const selectedModel = computed(() => drafts.modelId === undefined ? modelsField.value?.value ?? '' : drafts.modelId ?? '')
const fetching = ref(false)
const fetched = ref<{ value: string; label: string }[]>([])
const fetchRequest = new AbortController()
const dirty = computed(() => Object.keys(drafts).length > 0)

watch(() => props.fields, () => {
  for (const key of Object.keys(drafts)) delete drafts[key]
})

function updateDraft(key: string, next: string | null | undefined) {
  if (key === 'modelBaseUrl' || key === 'modelApiKey') fetched.value = []
  if (next === undefined) delete drafts[key]
  else drafts[key] = next
}

function edit(field: SettingField, next: string) {
  updateDraft(field.key, next === (field.value ?? '') ? undefined : next)
}

async function fetchModels() {
  if (fetching.value || props.saving) return
  fetching.value = true
  try {
    const { fetchModels } = await settingsApi.fetchModels(
      drafts.modelBaseUrl === null ? '' : drafts.modelBaseUrl,
      drafts.modelApiKey === null ? '' : drafts.modelApiKey,
      fetchRequest.signal,
    )
    fetched.value = fetchModels.map(({ id }) => ({ value: id, label: id }))
    if (!fetchRequest.signal.aborted) {
      notify.success(t(fetchModels.length ? 'composer.modelsLoaded' : 'composer.noModels'))
    }
  } catch (error) {
    if (!fetchRequest.signal.aborted) notify.error(messageFor(error, 'composer.modelsFailed'))
  } finally {
    fetching.value = false
  }
}

onUnmounted(() => fetchRequest.abort())

function save() {
  emit('save', Object.entries(drafts).map(([key, value]) => ({ key, value })))
}
</script>

<template>
  <UiSection
    :hidden="!group || (section === 'mcp' ? !mcp : !props.fields.length)"
    :aria-label="group ? t(group.label) : undefined"
  >
    <McpManager
      v-model="mcp"
      :hidden="section !== 'mcp'"
      :disabled="saving || fetching"
    />
    <UiForm :hidden="section === 'mcp'" @submit.prevent="save">
      <InstanceSettingField
        v-for="{ field, label } in displayedFields"
        :key="field.key"
        :field="field"
        :label="t(label)"
        :reset-label="section === 'model' ? t('settings.clearValue') : undefined"
        :draft="drafts[field.key]"
        :disabled="saving || fetching"
        @edit="edit(field, $event)"
        @clear="updateDraft(field.key, '')"
        @reset="updateDraft(field.key, null)"
      />
      <UiSection class="e2b-fields" :hidden="!e2b.length" :aria-label="t('sandbox.e2b')">
        <UiSubheading>{{ t('sandbox.e2b') }}</UiSubheading>
        <InstanceSettingField
          v-for="{ field, label } in e2b"
          :key="field.key"
          :field="field"
          :label="t(label)"
          :draft="drafts[field.key]"
          :disabled="saving || fetching"
          @edit="edit(field, $event)"
          @clear="drafts[field.key] = ''"
          @reset="drafts[field.key] = null"
        />
        <UiDisclosure class="connection-details">
          <template #summary>
            <UiIcon name="arrow-right-s-line" /> {{ t('settings.e2bConnection') }}
          </template>
          <InstanceSettingField
            v-for="{ field, label } in connection"
            :key="field.key"
            :field="field"
            :label="t(label)"
            :draft="drafts[field.key]"
            :disabled="saving || fetching"
            @edit="edit(field, $event)"
            @reset="drafts[field.key] = null"
          />
        </UiDisclosure>
      </UiSection>
      <UiBox class="model-discovery" :hidden="section !== 'model'">
        <UiChoiceDropdown
          :label="t('settings.defaultModel')"
          :options="fetched"
          :model-value="selectedModel"
          :placeholder="selectedModel || undefined"
          :disabled="saving || fetching"
          @update:model-value="modelsField && edit(modelsField, $event)"
        />
        <UiBox class="settings-input-actions">
          <UiButton class="secondary-button" :disabled="saving || fetching" @click="fetchModels">
            {{ t(fetching ? 'composer.fetchingModels' : 'composer.fetchModels') }}
          </UiButton>
          <UiButton
            class="secondary-button"
            :hidden="!modelsField?.overridden || drafts.modelId === null"
            :disabled="saving || fetching"
            @click="updateDraft('modelId', null)"
          >
            {{ t('settings.clearValue') }}
          </UiButton>
        </UiBox>
      </UiBox>
      <UiBox class="form-actions">
        <UiButton type="submit" :disabled="!dirty || saving || fetching">{{ t(saving ? 'settings.saving' : 'settings.save') }}</UiButton>
      </UiBox>
    </UiForm>
  </UiSection>
</template>

<style lang="less" scoped>
.e2b-fields { border-top: 1px solid var(--border); margin-top: 16px; padding-top: 16px; }
.e2b-fields h2 { margin-bottom: 12px; }
.settings-field + .settings-field { margin-top: 12px; }
.model-discovery:not([hidden]) { display: grid; gap: 8px; justify-items: start; margin-top: 12px; }
.model-discovery :deep(.field) { width: 100%; }
:deep(.settings-input-actions) { display: flex; flex-wrap: wrap; gap: 4px; }
:deep(.settings-input-actions > .secondary-button) { border: 1px solid var(--border-hover); }
:deep(.settings-field .settings-input-actions > .secondary-button) { margin-block-start: 6px; }
.connection-details { margin-top: 12px; }
.connection-details :deep(summary) { display: flex; align-items: center; gap: 4px; color: var(--muted); padding-block: 6px; }
.connection-details[open] :deep(summary .ui-icon) { transform: rotate(90deg); }
</style>
