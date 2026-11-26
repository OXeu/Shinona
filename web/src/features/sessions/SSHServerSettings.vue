<script setup lang="ts">
import { computed, onUnmounted, ref, useId, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiFilePicker from '../../shared/ui/UiFilePicker.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import UiSecretPaste from '../../shared/ui/UiSecretPaste.vue'
import UiTooltip from '../../shared/ui/UiTooltip.vue'
import { UiBox, UiButton, UiForm, UiInline, UiList, UiListItem, UiSection, UiSubheading } from '../../shared/ui/elements'
import { settingsApi } from './settingsApi'
import type { SSHServer, SSHServerInput } from './settingsApi'

const props = defineProps<{ active: boolean }>()
const servers = defineModel<SSHServer[]>({ required: true })
const prefix = useId()
const request = new AbortController()
const draft = ref<SSHServerInput>()
const removing = ref<SSHServer>()
const removeOpen = ref(false)
const submitting = ref(false)
const testing = ref<SSHServer>()
const busy = computed(() => submitting.value || !!testing.value)
const readingKey = ref(false)
const maxKeyBytes = 64 << 10
const hostFingerprintCommand = 'ssh-keygen -l -E sha256 -f /etc/ssh/ssh_host_ed25519_key.pub'
const canTest = computed(() => !!draft.value?.address.trim() &&
  !!draft.value.user.trim() && !!draft.value.hostFingerprint.trim() && (!!draft.value.id || !!draft.value.privateKey.trim()))
const canSave = computed(() => canTest.value && !!draft.value?.name.trim())

function edit(server?: SSHServer) {
  draft.value = {
    id: '', name: '', address: '', user: '', hostFingerprint: '', template: '',
    ...server, privateKey: '',
  }
}

async function readKey(file: File) {
  const input = draft.value
  if (!input || readingKey.value) return
  readingKey.value = true
  try {
    if (file.size > maxKeyBytes) throw new Error('key is too large')
    const key = await file.text()
    if (draft.value === input && props.active) input.privateKey = key
  } catch {
    if (props.active) notify.error(t('ssh.readKeyFailed'))
  } finally {
    readingKey.value = false
  }
}

async function save() {
  if (!draft.value || busy.value || !canSave.value) return
  const { signal } = request
  submitting.value = true
  try {
    const saveSshServer = await settingsApi.saveSSHServer(draft.value, signal)
    if (signal.aborted) return
    servers.value = [...servers.value.filter((server) => server.id !== saveSshServer.id), saveSshServer]
      .sort((a, b) => a.name.localeCompare(b.name))
    draft.value = undefined
    notify.success(t('settings.saved'))
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'settings.saveFailed'))
  } finally {
    submitting.value = false
  }
}

async function testConnection(server: SSHServer, privateKey = '') {
  if (busy.value || readingKey.value) return
  const { signal } = request
  testing.value = server
  try {
    await settingsApi.testSSHServer({ ...server, privateKey }, signal)
    if (!signal.aborted) notify.success(t('ssh.testSucceeded'))
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'ssh.testFailed'))
  } finally {
    testing.value = undefined
  }
}

function testDraft() {
  if (draft.value) return testConnection(draft.value, draft.value.privateKey)
}

async function remove() {
  if (!removing.value || busy.value) return
  const { signal } = request
  const id = removing.value.id
  submitting.value = true
  try {
    await settingsApi.removeSSHServer(id, signal)
    if (signal.aborted) return
    servers.value = servers.value.filter((server) => server.id !== id)
    if (draft.value?.id === id) draft.value = undefined
    removeOpen.value = false
    notify.success(t('ssh.removed'))
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'settings.saveFailed'))
  } finally {
    submitting.value = false
  }
}

watch(() => props.active, (active) => {
  if (!active) draft.value = undefined
})
onUnmounted(() => request.abort())
</script>

<template>
  <UiSection :hidden="!active" :aria-label="t('ssh.servers')">
    <UiList class="servers" :hidden="!servers.length">
      <UiListItem v-for="server in servers" :key="server.id" class="server">
        <UiBox>
          <UiInline class="server-name">{{ server.name }}</UiInline>
          <UiInline class="server-address">{{ server.user }}@{{ server.address }}</UiInline>
        </UiBox>
        <UiBox class="server-actions">
          <UiButton class="secondary-button" :disabled="busy || readingKey" @click="testConnection(server)">
            {{ t(testing === server ? 'ssh.testing' : 'ssh.test') }}
          </UiButton>
          <UiButton class="secondary-button" :disabled="busy" @click="edit(server)">{{ t('common.edit') }}</UiButton>
          <UiButton class="secondary-button" :disabled="busy" @click="removing = server; removeOpen = true">
            {{ t('common.remove') }}
          </UiButton>
        </UiBox>
      </UiListItem>
    </UiList>
    <UiButton :hidden="!!draft" :disabled="busy" @click="edit()">{{ t('ssh.add') }}</UiButton>
    <UiForm
      v-if="draft"
      @submit.prevent="save"
    >
      <UiSubheading>{{ t(draft.id ? 'ssh.edit' : 'ssh.add') }}</UiSubheading>
      <UiInput
        :id="`${prefix}-name`"
        v-model="draft.name"
        :label="t('ssh.name')"
        :disabled="busy"
        maxlength="160"
      />
      <UiInput
        :id="`${prefix}-host`"
        v-model="draft.address"
        :label="t('settings.sshAddress')"
        :disabled="busy"
        maxlength="255"
      />
      <UiInput
        :id="`${prefix}-user`"
        v-model="draft.user"
        :label="t('settings.sshUser')"
        :disabled="busy"
        maxlength="255"
      />
      <UiInput
        :id="`${prefix}-fingerprint`"
        v-model="draft.hostFingerprint"
        :label="t('settings.sshHostFingerprint')"
        placeholder="SHA256:…"
        :disabled="busy"
        maxlength="64"
      >
        <template #help>
          <UiTooltip
            :label="t('common.helpFor', { label: t('settings.sshHostFingerprint') })"
            :text="t('ssh.fingerprintHelp', { command: hostFingerprintCommand })"
          />
        </template>
      </UiInput>
      <UiInput
        :id="`${prefix}-image`"
        v-model="draft.template"
        :label="t('sandbox.image')"
        placeholder="buildpack-deps:bookworm-scm"
        :disabled="busy"
        maxlength="255"
      />
      <UiBox>
        <UiSecretPaste
          :id="`${prefix}-key`"
          v-model="draft.privateKey"
          :label="t('ssh.privateKey')"
          :placeholder="t(draft.id ? 'ssh.keepKey' : 'ssh.pasteKey')"
          :disabled="busy || readingKey"
          :max-bytes="maxKeyBytes"
          @too-large="notify.error(t('ssh.readKeyFailed'))"
        >
          <template #help>
            <UiTooltip
              :label="t('common.helpFor', { label: t('ssh.privateKey') })"
              :text="t('ssh.privateKeyHelp')"
            />
          </template>
        </UiSecretPaste>
        <UiFilePicker :label="t('ssh.selectKey')" :disabled="busy || readingKey" @select="readKey" />
      </UiBox>
      <UiBox class="form-actions">
        <UiButton class="secondary-button" :disabled="busy" @click="draft = undefined">{{ t('common.cancel') }}</UiButton>
        <UiButton class="secondary-button" :disabled="busy || readingKey || !canTest" @click="testDraft">
          {{ t(testing === draft ? 'ssh.testing' : 'ssh.test') }}
        </UiButton>
        <UiButton type="submit" :disabled="busy || readingKey || !canSave">{{ t(submitting ? 'settings.saving' : 'settings.save') }}</UiButton>
      </UiBox>
    </UiForm>
  </UiSection>
  <UiDialog v-model="removeOpen" :busy="busy" :label="t('ssh.remove')">
    <UiSubheading>{{ t('ssh.remove') }}</UiSubheading>
    <UiBox class="dialog-message">{{ t('ssh.confirmRemove', { name: removing?.name ?? '' }) }}</UiBox>
    <UiBox class="form-actions">
      <UiButton class="secondary-button" :disabled="busy" @click="removeOpen = false">{{ t('common.cancel') }}</UiButton>
      <UiButton :disabled="busy" @click="remove">{{ t('ssh.remove') }}</UiButton>
    </UiBox>
  </UiDialog>
</template>

<style scoped lang="less">
.servers { margin: 0 0 12px; padding: 0; list-style: none; }
.server { padding: 10px 0; border-bottom: 1px solid var(--border); }
.server, .server-actions { display: flex; justify-content: space-between; align-items: center; gap: 6px; flex-wrap: wrap; }
.server-name, .server-address { display: block; overflow-wrap: anywhere; }
.server-name { font-weight: 600; }
.server-address { color: var(--muted); }
</style>
