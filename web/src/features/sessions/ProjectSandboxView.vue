<script setup lang="ts">
import { computed, onUnmounted, ref, useId, watch } from 'vue'
import { RouterLink } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import type { MessageKey } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiChoiceDropdown from '../../shared/ui/UiChoiceDropdown.vue'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton, UiForm, UiHeading, UiInline, UiSection, UiSubheading } from '../../shared/ui/elements'
import { projectSandboxApi } from './projectSandboxApi'
import type { ProjectSandbox, ProjectSandboxConfig } from './projectSandboxApi'

const props = defineProps<{ projectId?: string }>()
type Action = 'save' | 'destroy' | 'recreate' | 'create'

const statusKeys: Record<ProjectSandbox['state'], MessageKey> = {
  NONE: 'sandbox.notCreated',
  RUNNING: 'sandbox.running',
  PAUSED: 'sandbox.paused',
  STOPPED: 'sandbox.stopped',
  MISSING: 'sandbox.missing',
  UNAVAILABLE: 'sandbox.statusUnavailable',
}
const templateId = useId()
const timeoutId = useId()
const snapshot = ref<ProjectSandbox>()
const loading = ref(false)
const busy = ref(false)
const confirmOpen = ref(false)
const action = ref<Action>('save')
const provider = ref('')
const template = ref('')
const timeout = ref('')
const onTimeout = ref('')
let request: AbortController

function providerName(name: string, label = '') {
  if (name === 'e2b') return t('sandbox.e2b')
  if (name === 'ssh') return t('sandbox.instanceSSH')
  if (label) return t('sandbox.sshServerLabel', { name: label })
  return t('common.unavailable')
}

function timeoutActionName(value: string) {
  return t(value === 'kill' ? 'sandbox.kill' : 'sandbox.pause')
}

const providerOptions = computed(() => {
  return (snapshot.value?.providers ?? []).map(({ provider, label }) => ({ value: provider, label: providerName(provider, label) }))
})
const defaults = computed(() => snapshot.value?.providers.find((item) => item.provider === provider.value))
const configured = computed(() => snapshot.value?.providers.some((item) => item.provider === snapshot.value?.effective.provider))
const timeoutOptions = computed(() => [
  { value: '', label: t('sandbox.inheritValue', { value: timeoutActionName(defaults.value?.onTimeout ?? '') }) },
  { value: 'pause', label: t('sandbox.pause') },
  { value: 'kill', label: t('sandbox.kill') },
])
const next = computed<ProjectSandboxConfig>(() => ({
  provider: provider.value,
  template: template.value.trim(),
  timeoutSeconds: provider.value === 'e2b' ? Number(timeout.value) : 0,
  onTimeout: provider.value === 'e2b' ? onTimeout.value : '',
}))
const validTimeout = computed(() => !timeout.value || provider.value !== 'e2b' ||
  /^\d+$/.test(timeout.value) && next.value.timeoutSeconds >= 1 && next.value.timeoutSeconds <= 86400)
const dirty = computed(() => {
  const current = snapshot.value
  const saved = current?.config
  const updated = next.value
  return !!saved && (providerFor(current) !== updated.provider || saved.template !== updated.template ||
    saved.timeoutSeconds !== updated.timeoutSeconds || saved.onTimeout !== updated.onTimeout)
})
const replaces = computed(() => {
  const current = snapshot.value
  const fallback = defaults.value
  if (!current || !fallback) return false
  const { effective } = current
  const updated = next.value
  return effective.provider !== fallback.provider || effective.template !== (updated.template || fallback.template) ||
    effective.timeoutSeconds !== (updated.timeoutSeconds || fallback.timeoutSeconds) ||
    effective.onTimeout !== (updated.onTimeout || fallback.onTimeout)
})
const recreateLabel = computed(() => t(snapshot.value?.state === 'NONE' ? 'sandbox.create' : 'sandbox.recreate'))
const confirmTitle = computed(() => action.value === 'save' ? t('settings.save') :
  action.value === 'destroy' ? t('sandbox.destroy') : recreateLabel.value)
const confirmMessage = computed(() => t(action.value === 'save' ? 'sandbox.confirmSave' :
  action.value === 'destroy' ? 'sandbox.confirmDestroy' : 'sandbox.confirmRecreate'))

function providerFor(state: ProjectSandbox | undefined) {
  return state?.config.provider || state?.effective.provider || ''
}

function restoreEditor(state: ProjectSandbox) {
  const { config } = state
  provider.value = providerFor(state)
  template.value = config.template
  timeout.value = config.timeoutSeconds ? String(config.timeoutSeconds) : ''
  onTimeout.value = config.onTimeout
}

async function refresh(reset = false) {
  const { signal } = request
  loading.value = true
  try {
    const { projectSandbox } = await projectSandboxApi.load(props.projectId ?? '', signal)
    if (!signal.aborted) {
      if (reset || !dirty.value) restoreEditor(projectSandbox)
      snapshot.value = projectSandbox
    }
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'sandbox.loadFailed'))
  } finally {
    if (!signal.aborted) loading.value = false
  }
}

function chooseProvider(value: string) {
  if (provider.value !== value) {
    provider.value = value
    template.value = ''
    timeout.value = ''
    onTimeout.value = ''
  }
}

function prepare(nextAction: Action) {
  if (busy.value || loading.value) return
  action.value = nextAction
  if (nextAction === 'save' && !replaces.value || nextAction === 'create') {
    void perform()
  } else {
    confirmOpen.value = true
  }
}

async function perform() {
  if (busy.value) return
  const { signal } = request
  const id = props.projectId ?? ''
  const operation = action.value
  busy.value = true
  try {
    if (operation === 'save') await projectSandboxApi.save(id, next.value, signal)
    else await projectSandboxApi[operation](id, signal)
    if (!signal.aborted) {
      confirmOpen.value = false
      notify.success(t(operation === 'save' ? 'settings.saved' : operation === 'destroy' ? 'sandbox.destroyed' : 'sandbox.created'))
      await refresh(operation === 'save')
    }
  } catch (error) {
    if (!signal.aborted) {
      notify.error(messageFor(error, 'sandbox.actionFailed'))
      await refresh()
    }
  } finally {
    if (!signal.aborted) busy.value = false
  }
}

watch(() => props.projectId, () => {
  request?.abort()
  request = new AbortController()
  snapshot.value = undefined
  busy.value = false
  confirmOpen.value = false
  void refresh(true)
}, { immediate: true })
onUnmounted(() => request.abort())
</script>

<template>
  <UiBox class="page sandbox-page">
    <RouterLink class="ghost-button back-link" :to="{ name: 'sessions' }">
      <UiIcon name="arrow-left-line" /> {{ t('sessions.heading') }}
    </RouterLink>
    <UiHeading>{{ t('sandbox.manage') }}</UiHeading>
    <UiBox class="project-name" :hidden="!snapshot">{{ snapshot?.project.name || t('projects.default') }}</UiBox>

    <UiSection class="sandbox-section" :aria-label="t('sandbox.status')">
      <UiBox class="status-header">
        <UiSubheading>{{ t('sandbox.status') }}</UiSubheading>
        <UiButton class="secondary-button" :disabled="busy || loading" @click="refresh()">
          {{ t(loading ? 'sandbox.refreshing' : 'sandbox.refresh') }}
        </UiButton>
      </UiBox>
      <UiBox class="status-body" :hidden="!snapshot">
        <UiInline class="state" :data-state="snapshot?.state">{{ snapshot ? t(statusKeys[snapshot.state]) : '' }}</UiInline>
        <UiInline>{{ providerName(snapshot?.effective.provider ?? '', snapshot?.providerLabel) }}</UiInline>
        <UiInline class="sandbox-id" :hidden="!snapshot?.sandboxId">{{ snapshot?.sandboxId }}</UiInline>
      </UiBox>
      <UiBox class="sandbox-actions" :hidden="!configured">
        <UiButton
          class="secondary-button"
          :disabled="busy || loading"
          @click="prepare(snapshot?.state === 'NONE' ? 'create' : 'recreate')"
        >
          {{ recreateLabel }}
        </UiButton>
        <UiButton
          class="secondary-button danger"
          :disabled="busy || loading || snapshot?.state === 'NONE'"
          @click="prepare('destroy')"
        >
          {{ t('sandbox.destroy') }}
        </UiButton>
      </UiBox>
    </UiSection>

    <UiSection class="sandbox-section" :hidden="!snapshot" :aria-label="t('sandbox.configuration')">
      <UiSubheading>{{ t('sandbox.configuration') }}</UiSubheading>
      <UiBox class="sandbox-hint" :hidden="!!providerOptions.length">
        <RouterLink :to="{ name: 'settings', query: { section: 'ssh' } }">{{ t('ssh.add') }}</RouterLink>
      </UiBox>
      <UiForm :hidden="!providerOptions.length" @submit.prevent="validTimeout && dirty && defaults && prepare('save')">
        <UiChoiceDropdown
          :model-value="provider"
          :label="t('sandbox.provider')"
          :placeholder="providerName(provider, snapshot?.providerLabel)"
          :options="providerOptions"
          :disabled="busy || loading"
          @update:model-value="chooseProvider"
        />
        <UiInput
          :id="templateId"
          v-model="template"
          :label="t(provider === 'e2b' ? 'sandbox.template' : 'sandbox.image')"
          :placeholder="defaults ? t('sandbox.inheritValue', { value: defaults.template }) : undefined"
          :disabled="busy || loading || !defaults"
          maxlength="255"
        />
        <UiBox :hidden="provider !== 'e2b'">
          <UiInput
            :id="timeoutId"
            v-model="timeout"
            :label="t('sandbox.timeout')"
            :placeholder="defaults ? t('sandbox.inheritValue', { value: defaults.timeoutSeconds }) : undefined"
            :disabled="busy || loading || !defaults"
            :aria-invalid="!validTimeout"
            inputmode="numeric"
            maxlength="5"
          />
          <UiChoiceDropdown
            v-model="onTimeout"
            :label="t('sandbox.onTimeout')"
            :options="timeoutOptions"
            :disabled="busy || loading || !defaults"
          />
          <UiBox class="sandbox-hint" :hidden="validTimeout">{{ t('sandbox.invalidTimeout') }}</UiBox>
        </UiBox>
        <UiBox class="form-actions">
          <UiButton type="submit" :disabled="busy || loading || !dirty || !defaults || !validTimeout">
            {{ t(busy && action === 'save' ? 'settings.saving' : 'settings.save') }}
          </UiButton>
        </UiBox>
      </UiForm>
    </UiSection>
  </UiBox>

  <UiDialog v-model="confirmOpen" :label="confirmTitle" :busy="busy">
    <UiSubheading>{{ confirmTitle }}</UiSubheading>
    <UiForm @submit.prevent="perform">
      <UiBox class="dialog-message">{{ confirmMessage }}</UiBox>
      <UiBox class="form-actions">
        <UiButton class="secondary-button" :disabled="busy" @click="confirmOpen = false">{{ t('common.cancel') }}</UiButton>
        <UiButton type="submit" :disabled="busy">{{ confirmTitle }}</UiButton>
      </UiBox>
    </UiForm>
  </UiDialog>
</template>

<style lang="less" scoped>
.back-link { display: inline-flex; gap: 5px; align-items: center; margin: 0 0 12px -8px; padding: 4px 8px; }
.project-name { margin-top: 4px; color: var(--muted); }
.sandbox-section { margin-top: 20px; padding: 16px; border: 1px solid var(--border); border-radius: var(--radius-lg); }
.status-header, .status-body:not([hidden]), .sandbox-actions:not([hidden]) { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.status-header { justify-content: space-between; }
.status-body:not([hidden]), .sandbox-actions:not([hidden]), .sandbox-hint { margin-top: 10px; }
.sandbox-hint, .status-body { color: var(--muted); }
.state { padding: 3px 8px; border-radius: var(--radius-full); background: var(--selection-bg); color: var(--foreground); }
.sandbox-id { font-family: monospace; overflow-wrap: anywhere; }
.sandbox-actions button { border: 1px solid var(--border); }
.sandbox-actions .danger { color: var(--danger); }
</style>
