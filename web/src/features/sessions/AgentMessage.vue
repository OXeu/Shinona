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
  const summary = completed.value && chunk === lastProcess.value
    ? { text: elapsed.value, icon: 'time-line' }
    : activity(chunk.parts, !settled.value && chunk === chunks.value.at(-1))
  return { ...chunk, summary }
}))

function activity(parts: TurnPart[], active: boolean) {
  const categories = activityCategories(parts, active)
  const labels = categories.slice(0, 3).map((category) => t(`chat.activity.${category}`))
  const latest = parts.at(-1)
  const estimatedTokens = latest?.kind === 'thinking' ? latest.estimatedTokens : undefined
  if (active && categories[0] === 'thinking' && estimatedTokens) {
    return {
      icon: activityIcons.thinking,
      text: t('chat.activeThinkingTokens', { count: new Intl.NumberFormat(languageTag()).format(estimatedTokens) }),
    }
  }
  return {
    icon: activityIcons[categories[0] ?? 'thinking'],
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
          v-for="part in chunk.kind === 'text' ? [chunk.part] : []"
          :key="part.index"
          class="agent-answer"
          :text="part.text"
        />
        <AgentRichContent
          v-for="part in chunk.kind === 'ui' ? [chunk.part] : []"
          :key="part.index"
          :format="part.tool"
          :payload="part.output"
          :summary="part.text"
        />
        <UiBox v-for="group in chunk.kind === 'process' ? [chunk] : []" :key="group.index">
          <UiButton
            class="agent-process-toggle"
            :aria-expanded="openGroups.has(group.index)"
            :aria-controls="processId + '-' + group.index"
            @click="toggleGroup(group.index)"
          >
            <UiIcon :name="group.summary.icon" />
            {{ group.summary.text }}
            <UiIcon :name="openGroups.has(group.index) ? 'arrow-down-s-line' : 'arrow-right-s-line'" />
          </UiButton>
          <UiBox :id="processId + '-' + group.index" class="agent-process-member" :hidden="!openGroups.has(group.index)">
            <template v-for="part in openGroups.has(group.index) ? group.parts : []" :key="part.index">
              <UiMarkdown
                v-for="text in part.kind === 'text' ? [part] : []"
                :key="text.index"
                class="agent-answer"
                :text="text.text"
              />
              <UiDisclosure v-for="thought in part.kind === 'thinking' ? [part] : []" :key="thought.index">
                <template #summary>
                  <UiBox class="agent-part-heading">
                    <UiIcon class="agent-disclosure-chevron" name="arrow-right-s-line" />
                    <UiIcon name="brain-line" /> {{ t('chat.activity.thinking') }}
                  </UiBox>
                </template>
                <UiMarkdown class="agent-part-detail" :text="thought.text" />
              </UiDisclosure>
              <AgentToolCall
                v-for="tool in part.kind === 'tool' ? [part] : []"
                :key="tool.index"
                :part="tool"
              />
            </template>
          </UiBox>
        </UiBox>
      </template>
    </UiBox>

    <UiMarkdown class="agent-answer" :hidden="!turn.output" :text="turn.output" />
    <UiBox class="agent-message-meta" :hidden="!settled">
      <UiInline :hidden="!tokenUsage" class="agent-token-usage">{{ tokenUsage }}</UiInline>
      <UiTime :datetime="turn.updatedAt" :title="formatTime(turn.updatedAt)">{{ formatRelativeTime(turn.updatedAt, now, { maxDays: 3 }) }}</UiTime>
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
