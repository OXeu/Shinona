<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../../shared/i18n'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiInline, UiList, UiListItem, UiPre, UiSection, UiSubheading } from '../../shared/ui/elements'
import type { PendingAction, PendingMessage } from './sessionApi'

const props = defineProps<{ messages: PendingMessage[]; hasActiveTurn: boolean; busy: boolean }>()
const emit = defineEmits<{
  action: [message: PendingMessage, action: PendingAction]
  copy: [prompt: string, attachments?: string[]]
}>()
const groups = computed(() => [
  { kind: 'steering', label: t('composer.steeringGroup'), messages: props.messages.filter(({ kind }) => kind === 'steering') },
  { kind: 'queue', label: t('composer.queueGroup'), messages: props.messages.filter(({ kind }) => kind === 'queue') },
].filter(({ messages }) => messages.length))
</script>

<template>
  <UiSection
    :hidden="!groups.length"
    class="pending-messages"
    tabindex="0"
    :aria-label="t('composer.pendingMessages')"
  >
    <UiBox v-for="group in groups" :key="group.kind" class="pending-group">
      <UiSubheading class="pending-heading">{{ group.label }}</UiSubheading>
      <UiList class="pending-list" role="list" :aria-label="group.label">
        <UiListItem v-for="message in group.messages" :key="message.id" class="pending-message">
          <UiPre :hidden="!message.prompt" class="pending-text">{{ message.prompt }}</UiPre>
          <UiInline :hidden="message.kind === 'steering' || !message.attachments.length" class="pending-attachments">
            {{ t('composer.attached', { images: message.kind === 'queue' ? message.attachments.join(', ') : '' }) }}
          </UiInline>
          <UiBox class="pending-actions">
            <UiIconButton
              :icon="message.kind === 'steering' ? 'list-check-3' : 'flashlight-line'"
              :label="t(message.kind === 'steering' ? 'composer.moveToQueue' : 'composer.moveToSteering')"
              :title="t(message.kind === 'steering' ? 'composer.moveToQueue' : !hasActiveTurn ? 'composer.notRunning' :
                message.attachments.length ? 'composer.steerTextOnly' : 'composer.moveToSteering')"
              :disabled="busy || (message.kind === 'queue' && (!hasActiveTurn || !!message.attachments.length))"
              @click="emit('action', message, 'move')"
            />
            <UiIconButton
              icon="arrow-go-back-line"
              :label="t('composer.returnToDraft')"
              :disabled="busy"
              @click="emit('action', message, 'draft')"
            />
            <UiIconButton
              icon="file-copy-line"
              :label="t('chat.copyMessage')"
              @click="emit('copy', message.prompt, message.kind === 'queue' ? message.attachments : undefined)"
            />
            <UiIconButton
              icon="delete-bin-line"
              :label="t('composer.deletePending')"
              :disabled="busy"
              @click="emit('action', message, 'delete')"
            />
          </UiBox>
        </UiListItem>
      </UiList>
    </UiBox>
  </UiSection>
</template>

<style src="./styles/pending-messages.less" lang="less" scoped></style>
