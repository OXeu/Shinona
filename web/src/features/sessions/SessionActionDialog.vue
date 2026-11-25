<script setup lang="ts">
import { computed, onUnmounted, ref, useId } from 'vue'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton, UiForm, UiSubheading } from '../../shared/ui/elements'
import { sessionTitle } from './presentation'
import type { SessionAction } from './presentation'
import { sessionApi } from './sessionApi'
import type { SessionSummary } from './sessionApi'
import { useSessionStore } from './sessionStore'

const emit = defineEmits<{ updated: [session: SessionSummary]; archived: [id: string] }>()
const store = useSessionStore()
const request = new AbortController()
const inputId = useId()
const dialogOpen = ref(false)
const dialogAction = ref<'rename' | 'remove'>('rename')
const target = ref<SessionSummary>()
const title = ref('')
const saving = ref(false)
const dialogTitle = computed(() => t(dialogAction.value === 'rename' ? 'sessions.renameTitle' : 'sessions.removeTitle'))

function handleUpdate(session: SessionSummary) {
  store.upsert(session)
  emit('updated', session)
}

async function perform(action: SessionAction, item: SessionSummary) {
  if (saving.value) return
  if (action !== 'pin') {
    target.value = item
    title.value = sessionTitle(item)
    dialogAction.value = action
    dialogOpen.value = true
    return
  }

  saving.value = true
  try {
    const { updateSession } = await sessionApi.update(item.id, { pinned: !item.pinned })
    if (request.signal.aborted) return
    handleUpdate(updateSession)
    notify.success(t(updateSession.pinned ? 'sessions.pinnedToast' : 'sessions.unpinnedToast'))
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, 'sessions.pinFailed'))
  } finally {
    saving.value = false
  }
}

async function save() {
  const item = target.value
  if (!item || saving.value) return
  if (dialogAction.value === 'rename' && !title.value.trim()) {
    notify.error(t('sessions.enterTitle'))
    return
  }
  saving.value = true
  try {
    if (dialogAction.value === 'remove') {
      await sessionApi.archive(item.id)
      if (request.signal.aborted) return
      store.remove(item.id)
      emit('archived', item.id)
      notify.success(t('sessions.removed'))
    } else {
      const { updateSession } = await sessionApi.update(item.id, { title: title.value })
      if (request.signal.aborted) return
      handleUpdate(updateSession)
      notify.success(t('sessions.renamed'))
    }
    dialogOpen.value = false
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, 'sessions.updateFailed'))
  } finally {
    saving.value = false
  }
}

defineExpose({ perform })
onUnmounted(() => request.abort())
</script>

<template>
  <UiDialog v-model="dialogOpen" :label="dialogTitle">
    <UiSubheading>{{ dialogTitle }}</UiSubheading>
    <UiForm @submit.prevent="save">
      <UiBox :hidden="dialogAction !== 'rename'">
        <UiInput
          :id="inputId"
          v-model="title"
          :label="t('sessions.title')"
          maxlength="80"
        />
      </UiBox>
      <UiBox class="dialog-message" :hidden="dialogAction !== 'remove'">{{ t('sessions.archiveConfirm', { title: target ? sessionTitle(target) : '' }) }}</UiBox>
      <UiBox class="form-actions">
        <UiButton class="secondary-button" :disabled="saving" @click="dialogOpen = false">{{ t('common.cancel') }}</UiButton>
        <UiButton type="submit" :disabled="saving">{{ t(dialogAction === 'rename' ? 'sessions.saveTitle' : 'sessions.removeTitle') }}</UiButton>
      </UiBox>
    </UiForm>
  </UiDialog>
</template>
