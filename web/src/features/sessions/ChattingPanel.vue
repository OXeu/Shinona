<script setup lang="ts">
import { computed, onUnmounted, ref, useTemplateRef, watch } from 'vue'

import { copyText } from '../../shared/clipboard'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiChatFrame from '../../shared/ui/UiChatFrame.vue'
import UiChoiceDropdown from '../../shared/ui/UiChoiceDropdown.vue'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import UiVirtualColumn from '../../shared/ui/UiVirtualColumn.vue'
import { UiBox, UiButton, UiImage, UiPre, UiSection, UiSubheading } from '../../shared/ui/elements'
import AgentComposer from './AgentComposer.vue'
import AgentMessage from './AgentMessage.vue'
import ChatSystemMessage from './ChatSystemMessage.vue'
import SessionList from './SessionList.vue'
import SessionProjectPicker from './SessionProjectPicker.vue'
import type { SessionAction } from './presentation'
import type {
  AgentSession, ComposerAction, ComposerDefaults, ComposerOptions, ComposerPayload, ComposerRunOptions,
  ImageAttachment, PendingAction, PendingMessage, SessionSummary, Turn,
} from './sessionApi'
import { attachmentURL } from './sessionApi'

const props = defineProps<{
  session: AgentSession | null
  recentSessions: SessionSummary[]
  now: number
  hasSelection: boolean
  busy: boolean
  reverting: boolean
  catalog: ComposerOptions
  savingDefaults: boolean
  hasOlder: boolean
  loadingOlder: boolean
  refreshPaused: boolean
  errors: Readonly<Partial<Record<string, string>>>
}>()
const emit = defineEmits<{
  submit: [action: ComposerAction, payload: ComposerPayload]
  stop: [id: string]
  pending: [message: PendingMessage, action: PendingAction, runId: string | undefined, options: ComposerRunOptions]
  revert: [id: string]
  retry: [id: string, options: ComposerRunOptions]
  saveDefaults: [options: ComposerDefaults]
  loadOlder: []
  refresh: []
  error: [source: 'copy' | 'attachment', message: string]
  action: [action: SessionAction, entry: SessionSummary]
}>()
const prompt = defineModel<string>({ required: true })
const images = defineModel<ImageAttachment[]>('images', { required: true })
const centered = computed(() => !props.hasSelection || (!!props.session && !props.session.turns.length))
const recent = computed(() => props.recentSessions
  .filter((item) => item.id !== props.session?.id)
  .toSorted((a, b) => b.updatedAt.localeCompare(a.updatedAt))
  .slice(0, 5))

type Message = { id: string } & (
  | { kind: 'history'; gap: boolean }
  | { kind: 'user'; text: string; turnId?: string; attachments?: string[] }
  | { kind: 'agent'; turn: Turn }
  | { kind: 'system'; text: string; tone?: 'error'; failedTurn?: Turn }
)

const failureDialogOpen = ref(false)
const failureDetail = ref('')
const failureTurnId = ref('')
const retryModel = ref('')
const copiedId = ref<string | null>(null)
let copiedTimer: number | undefined
const composer = useTemplateRef('composer')
const retryModels = computed(() => [
  { value: '', label: t('chat.retryCurrentModel') },
  ...props.catalog.models.map(({ id }) => ({ value: id, label: id })),
])
const canRetryFromDetails = computed(() => !!failureTurnId.value &&
  props.session?.turns.at(-1)?.id === failureTurnId.value && retryModels.value.length > 1)
watch(() => props.session?.id, () => {
  failureDialogOpen.value = false
  copiedId.value = null
  window.clearTimeout(copiedTimer)
})
onUnmounted(() => window.clearTimeout(copiedTimer))

function failurePart(turn: Turn) {
  return turn.parts.findLast((part) => part.kind === 'failure')
}

function showFailureDetails(turn: Turn) {
  failureDetail.value = failurePart(turn)?.output || t('chat.failureDetailsUnavailable')
  failureTurnId.value = turn.id
  retryModel.value = ''
  failureDialogOpen.value = true
}

function retry(turnId: string, model?: string) {
  const options = composer.value?.runOptions
  if (options) emit('retry', turnId, { ...options, model: model || options.model })
}

function retryFromDetails() {
  if (!canRetryFromDetails.value || props.busy || props.session?.active) return
  failureDialogOpen.value = false
  retry(failureTurnId.value, retryModel.value)
}

function failureMessage(turn: Turn) {
  switch (failurePart(turn)?.text) {
    case 'deadline': return t('chat.failedDeadline')
    case 'interrupted': return t('chat.failedInterrupted')
    case 'model_idle': return t('chat.failedModelIdle')
    case 'model': return t('chat.failedModel')
    case 'machine': return t('chat.failedSandbox')
    case 'sandbox': return t('chat.failedSandbox')
    default: return t('chat.failed')
  }
}

async function copyMessage(text: string, attachments?: string[], id?: string) {
  try {
    const names = attachments?.length ? t('composer.attached', { images: attachments.join(', ') }) : ''
    await copyText([text, names].filter(Boolean).join('\n'))
    emit('error', 'copy', '')
    if (id) {
      copiedId.value = id
      window.clearTimeout(copiedTimer)
      copiedTimer = window.setTimeout(() => {
        if (copiedId.value === id) copiedId.value = null
      }, 1400)
    }
    notify.success(t('chat.messageCopied'))
  } catch {
    emit('error', 'copy', t('chat.copyMessageFailed'))
  }
}

function appendTurn(rows: Message[], turn: Turn) {
  if (turn.status === 'QUEUED') return
  rows.push({ id: turn.id + '/prompt', kind: 'user', turnId: turn.id, text: turn.prompt, attachments: turn.attachments })
  for (const [index, steer] of turn.steering.entries()) {
    if (!steer.delivered && turn.status === 'RUNNING') continue
    const key = turn.id + '/steer-' + index
    rows.push({ id: key, kind: 'user', text: steer.text })
    if (!steer.delivered) {
      rows.push({
        id: key + '/state', kind: 'system',
        text: t('composer.steerUndelivered'),
      })
    }
  }
  if (turn.output || turn.parts.some((part) => part.kind !== 'failure')) {
    rows.push({ id: turn.id + '/reply', kind: 'agent', turn })
  }
  if (turn.status === 'FAILED' || turn.status === 'STOPPED') {
    rows.push({
      id: turn.id + '/state', kind: 'system',
      text: turn.status === 'FAILED' ? failureMessage(turn) : t('composer.stopped'),
      tone: turn.status === 'FAILED' ? 'error' : undefined,
      failedTurn: turn.status === 'FAILED' ? turn : undefined,
    })
  }
}

const messages = computed(() => {
  const rows: Message[] = []
  const turns = props.session?.turns ?? []
  const earlier = props.session?.earlierTurn
  if (earlier) appendTurn(rows, earlier)
  if (props.hasOlder) {
    const first = turns[0]
    rows.push({ id: 'history', kind: 'history', gap: !!earlier && !!first && earlier.sequence + 1 < first.sequence })
  }
  for (const turn of turns) appendTurn(rows, turn)
  for (const [source, text] of Object.entries(props.errors)) {
    if (text) rows.push({ id: 'error/' + source, kind: 'system', tone: 'error', text })
  }
  return rows
})
</script>

<template>
  <UiChatFrame class="conversation" :composer-visible="!hasSelection || !!session" :centered="centered">
    <template #default="{ inset }">
      <UiVirtualColumn
        :key="session?.id || 'draft'"
        :label="t('chat.conversation')"
        :items="messages"
        :item-key="(item) => item.id"
        :bottom-inset="inset"
        :fit-content="centered"
      >
        <template #default="{ item }">
          <UiBox class="message-row" :data-kind="item.kind">
            <UiBox v-if="item.kind === 'history'" class="conversation-history">
              <UiButton class="secondary-button" :disabled="loadingOlder || reverting" @click="emit('loadOlder')">
                {{ t(loadingOlder ? 'chat.loadingOlder' : item.gap ? 'chat.loadHistoryGap' : 'chat.loadOlder') }}
              </UiButton>
            </UiBox>
            <UiBox v-else-if="item.kind === 'user'" class="user-message">
              <UiBox class="message-body">
                <UiPre :hidden="!item.text">{{ item.text }}</UiPre>
                <UiBox class="user-message-images" :hidden="!item.attachments?.length">
                  <UiImage
                    v-for="(name, index) in item.attachments"
                    :key="index"
                    class="user-message-image"
                    :src="session && item.turnId ? attachmentURL(session.id, item.turnId, index) : undefined"
                    :alt="name"
                    loading="lazy"
                    decoding="async"
                  />
                </UiBox>
              </UiBox>
              <UiBox class="user-message-actions">
                <UiIconButton
                  class="message-action user-message-revert"
                  icon="arrow-go-back-line"
                  :label="t('chat.revertMessage')"
                  :hidden="!item.turnId"
                  :disabled="busy || !!session?.active"
                  @click="item.turnId && emit('revert', item.turnId)"
                />
                <UiIconButton
                  class="message-action user-message-copy"
                  :icon="copiedId === item.id ? 'check-line' : 'file-copy-line'"
                  :label="t('chat.copyMessage')"
                  :data-copied="copiedId === item.id || undefined"
                  @click="copyMessage(item.text, item.attachments, item.id)"
                />
              </UiBox>
            </UiBox>
            <AgentMessage
              v-else-if="item.kind === 'agent'"
              :turn="item.turn"
              :now="now"
              @error="emit('error', 'copy', $event)"
            />
            <ChatSystemMessage
              v-else
              :text="item.text"
              :tone="item.tone"
              :retryable="item.id === 'error/load' ? refreshPaused : !!item.failedTurn && item.failedTurn.id === session?.turns.at(-1)?.id"
              :retry-disabled="item.id !== 'error/load' && (busy || !!session?.active)"
              :retry-label="item.id === 'error/load' ? t('common.retry') : undefined"
              :show-details="!!item.failedTurn"
              @retry="item.failedTurn ? retry(item.failedTurn.id) : emit('refresh')"
              @details="item.failedTurn && showFailureDetails(item.failedTurn)"
            />
          </UiBox>
        </template>
        <template #empty>
          <UiBox class="conversation-intro" :hidden="hasSelection && (!session || !!session.turns.length)">
            <UiSubheading>{{ t(hasSelection ? 'chat.firstPrompt' : 'chat.start') }}</UiSubheading>
          </UiBox>
        </template>
      </UiVirtualColumn>
    </template>

    <template #before-composer>
      <UiBox class="welcome-bar" :hidden="!centered">
        <UiImage
          class="welcome-peek"
          src="/shinona-logo.webp"
          alt=""
          width="38"
          height="38"
        />
        <SessionProjectPicker v-if="!hasSelection" hide-all />
      </UiBox>
    </template>
    <template #composer>
      <AgentComposer
        :key="session?.id"
        ref="composer"
        v-model="prompt"
        v-model:images="images"
        :busy="busy"
        :draft-locked="reverting"
        :session="session"
        :catalog="catalog"
        :saving-defaults="savingDefaults"
        :draft="!hasSelection"
        @send="(action, payload) => emit('submit', action, payload)"
        @stop="emit('stop', $event)"
        @pending="(message, action, runId, options) => emit('pending', message, action, runId, options)"
        @copy="copyMessage"
        @save-defaults="emit('saveDefaults', $event)"
        @error="emit('error', 'attachment', $event)"
      />
    </template>
    <template #after-composer>
      <UiSection class="welcome-recent" :aria-label="t('sessions.recent')" :hidden="!centered || !recent.length">
        <SessionList
          :entries="recent"
          :now="now"
          :label="t('sessions.recent')"
          @action="(action, entry) => emit('action', action, entry)"
        />
      </UiSection>
    </template>
  </UiChatFrame>
  <UiDialog v-model="failureDialogOpen" :label="t('chat.failureDetails')">
    <UiSubheading>{{ t('chat.failureDetails') }}</UiSubheading>
    <UiPre class="failure-detail">{{ failureDetail }}</UiPre>
    <UiBox :hidden="!canRetryFromDetails">
      <UiChoiceDropdown v-model="retryModel" :label="t('chat.retryModel')" :options="retryModels" />
    </UiBox>
    <UiBox class="form-actions">
      <UiButton class="secondary-button" @click="failureDialogOpen = false">{{ t('common.close') }}</UiButton>
      <UiButton
        :hidden="!canRetryFromDetails"
        :disabled="busy || !!session?.active"
        @click="retryFromDetails"
      >
        {{ t('chat.retryTurn') }}
      </UiButton>
    </UiBox>
  </UiDialog>
</template>

<style lang="less" scoped>
.conversation {
  --composer-max-width: 42rem;
  --composer-padding-inline: 12px;
  --composer-border-width: 1px;
}

.conversation-history {
  display: grid;
  justify-items: center;
  gap: 0.5rem;
  margin-inline: auto;
  padding-block-start: 1rem;
}

.failure-detail {
  max-block-size: 50vh;
  overflow: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
