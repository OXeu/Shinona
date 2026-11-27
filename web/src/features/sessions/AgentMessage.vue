<script setup lang="ts">
import { computed, defineAsyncComponent, ref, useId, watch } from 'vue'

import { copyText } from '../../shared/clipboard'
import { languageTag, t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { formatRelativeTime, formatTime } from '../../shared/time'
import UiDisclosure from '../../shared/ui/UiDisclosure.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import UiMarkdown from '../../shared/ui/UiMarkdown'
import { UiBox, UiButton, UiInline, UiTime } from '../../shared/ui/elements'
import AgentActivityLabel from './AgentActivityLabel.vue'
import AgentToolCall from './AgentToolCall.vue'
import { activityCategories, activityIcons, hasSettled, messageChunks } from './agentMessageParts'
import { formatCompactTokens } from './presentation'
import type { Turn, TurnPart } from './sessionApi'

const props = defineProps<{ turn: Turn; now: number }>()
const AgentRichContent = defineAsyncComponent(() => import('./AgentRichContent.vue'))
const emit = defineEmits<{ error: [message: string] }>()
const processId = useId()
const openGroups = ref(new Set<number>())
const settled = computed(() => hasSettled(props.turn))
watch(settled, (isSettled) => {
  if (isSettled) openGroups.value.clear()
})
const completed = computed(() => props.turn.status === 'SUCCEEDED')
const tokenUsage = computed(() => {
  const usage = props.turn.tokenUsage
  return usage ? t('chat.tokenUsage', {
    input: formatCompactTokens(usage.input),
    cached: formatCompactTokens(usage.cached),
    output: formatCompactTokens(usage.output),
  }) : ''
})
const chunks = computed(() => messageChunks(props.turn))
const lastProcess = computed(() => chunks.value.findLast((chunk) => chunk.kind === 'process'))
const waiting = computed(() => props.turn.status === 'RUNNING' && chunks.value.at(-1)?.kind !== 'process')
const elapsed = computed(() => {
  if (!props.turn.startedAt) return t('chat.processCompleted')
  const duration = Date.parse(props.turn.updatedAt) - Date.parse(props.turn.startedAt)
  if (!Number.isFinite(duration) || duration < 0) return t('chat.processCompleted')
  const seconds = Math.max(1, Math.round(duration / 1000))
  const number = new Intl.NumberFormat(languageTag())
  return t('chat.processDuration', { minutes: number.format(Math.floor(seconds / 60)), seconds: number.format(seconds % 60) })
})
const displayedChunks = computed(() => chunks.value.map((chunk) => {
  if (chunk.kind !== 'process') return chunk
  const active = !settled.value && chunk === chunks.value.at(-1)
  const summary = completed.value && chunk === lastProcess.value
    ? { text: elapsed.value, icon: 'time-line' }
    : activity(chunk.parts, active)
  return { ...chunk, summary, animate: active && props.turn.status === 'RUNNING' }
}))

function activity(parts: TurnPart[], active: boolean) {
  const categories = activityCategories(parts, active)
  const category = categories[0] ?? 'thinking'
  const labels = categories.slice(0, 3).map((category) => t(`chat.activity.${category}`))
  const latest = parts.at(-1)
  const estimatedTokens = latest?.kind === 'thinking' ? latest.estimatedTokens : undefined
  if (active && category === 'thinking' && estimatedTokens) {
    return {
      icon: activityIcons.thinking,
      text: t('chat.activeThinkingTokens', { count: new Intl.NumberFormat(languageTag()).format(estimatedTokens) }),
    }
  }
  return {
    icon: activityIcons[category],
    text: active ? t('chat.activeActivity', { activity: labels[0] ?? '' }) : new Intl.ListFormat(languageTag()).format(labels),
  }
}

function toggleGroup(index: number) {
  if (openGroups.value.has(index)) openGroups.value.delete(index)
  else openGroups.value.add(index)
}

async function copy() {
  try {
    await copyText(props.turn.output)
    emit('error', '')
    notify.success(t('chat.answerCopied'))
  } catch {
    emit('error', t('chat.copyFailed'))
  }
}
</script>

<template>
  <UiBox class="agent-message">
    <UiInline class="agent-completion-label" :hidden="!completed || !!lastProcess">{{ elapsed }}</UiInline>

    <UiBox
      class="agent-process"
      :hidden="!displayedChunks.length"
    >
      <template v-for="chunk in displayedChunks" :key="chunk.kind === 'process' ? chunk.index : chunk.part.index">
        <UiMarkdown
          v-if="chunk.kind === 'text'"
          class="agent-answer"
          :text="chunk.part.text ?? ''"
        />
        <AgentRichContent
          v-else-if="chunk.kind === 'ui'"
          :format="chunk.part.tool ?? ''"
          :payload="chunk.part.output ?? ''"
          :summary="chunk.part.text ?? ''"
        />
        <UiBox v-else-if="chunk.kind === 'process'">
          <UiButton
            class="agent-process-toggle"
            :aria-label="chunk.summary.text"
            :aria-expanded="openGroups.has(chunk.index)"
            :aria-controls="processId + '-' + chunk.index"
            @click="toggleGroup(chunk.index)"
          >
            <UiIcon :name="chunk.summary.icon" />
            <AgentActivityLabel
              v-if="chunk.animate"
              :text="chunk.summary.text"
            />
            <UiInline v-else>{{ chunk.summary.text }}</UiInline>
            <UiIcon :name="openGroups.has(chunk.index) ? 'arrow-down-s-line' : 'arrow-right-s-line'" />
          </UiButton>
          <UiBox :id="processId + '-' + chunk.index" class="agent-process-member" :hidden="!openGroups.has(chunk.index)">
            <template v-for="part in openGroups.has(chunk.index) ? chunk.parts : []" :key="part.index">
              <UiMarkdown
                v-if="part.kind === 'text'"
                class="agent-answer"
                :text="part.text ?? ''"
              />
              <UiDisclosure v-else-if="part.kind === 'thinking'">
                <template #summary>
                  <UiBox class="agent-part-heading">
                    <UiIcon class="agent-disclosure-chevron" name="arrow-right-s-line" />
                    <UiIcon name="brain-line" /> {{ t('chat.activity.thinking') }}
                  </UiBox>
                </template>
                <UiMarkdown class="agent-part-detail" :text="part.text ?? ''" />
              </UiDisclosure>
              <AgentToolCall
                v-else-if="part.kind === 'tool'"
                :part="part"
              />
            </template>
          </UiBox>
        </UiBox>
      </template>
    </UiBox>

    <UiBox
      :hidden="!waiting"
      class="agent-current-activity"
      role="group"
      :aria-label="t('chat.activeActivity', { activity: t('chat.activity.thinking') })"
    >
      <UiIcon :name="activityIcons.thinking" />
      <AgentActivityLabel :text="t('chat.activeActivity', { activity: t('chat.activity.thinking') })" />
    </UiBox>
    <UiMarkdown class="agent-answer" :hidden="!turn.output" :text="turn.output" />
    <UiBox class="agent-message-meta" :hidden="!settled">
      <UiInline :hidden="!tokenUsage" class="agent-token-usage">{{ tokenUsage }}</UiInline>
      <UiTime :datetime="turn.updatedAt" :title="formatTime(turn.updatedAt)">
        {{ formatRelativeTime(turn.updatedAt, now, { maxDays: 3 }) }}
      </UiTime>
      <UiIconButton
        class="message-action agent-copy"
        icon="file-copy-line"
        :label="t('chat.copyAnswer')"
        :hidden="!turn.output"
        @click="copy"
      />
    </UiBox>
  </UiBox>
</template>

<style src="./styles/agent-message.less" lang="less"></style>
