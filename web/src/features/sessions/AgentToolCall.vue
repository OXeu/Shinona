<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../../shared/i18n'
import UiDisclosure from '../../shared/ui/UiDisclosure.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import { UiBox, UiInline, UiPre } from '../../shared/ui/elements'
import { activityIcons, toolCategory } from './agentMessageParts'
import type { TurnPart } from './sessionApi'

const props = defineProps<{ part: TurnPart }>()
const category = computed(() => toolCategory(props.part.tool))
const failed = computed(() => props.part.status === 'FAILED')
const parsedInput = computed(() => {
  if (!props.part.input) return null
  try {
    return JSON.parse(props.part.input) as { path?: unknown; command?: unknown } | null
  } catch {
    return null
  }
})
const detail = computed(() => {
  const preview = category.value === 'command' ? parsedInput.value?.command : parsedInput.value?.path
  return typeof preview === 'string' ? preview : ''
})
const input = computed(() => parsedInput.value ? JSON.stringify(parsedInput.value, null, 2) : props.part.input)
</script>

<template>
  <UiDisclosure class="agent-tool">
    <template #summary>
      <UiBox class="agent-part-heading">
        <UiIcon class="agent-disclosure-chevron" name="arrow-right-s-line" />
        <UiIcon :name="activityIcons[category]" />
        <UiInline class="agent-tool-name">{{ part.tool }}</UiInline>
        <UiInline class="agent-tool-preview" :title="detail">{{ detail }}</UiInline>
        <UiInline class="agent-tool-status" :hidden="!failed">
          {{ t('run.failed') }}
        </UiInline>
      </UiBox>
    </template>
    <UiBox class="agent-part-detail" :data-terminal="category === 'command'">
      <UiBox :hidden="!input">
        <UiInline class="agent-part-label">{{ t('chat.toolInput') }}</UiInline>
        <UiPre>{{ input }}</UiPre>
      </UiBox>
      <UiBox :hidden="!part.output">
        <UiInline class="agent-part-label">{{ t('chat.toolOutput') }}</UiInline>
        <UiPre>{{ part.output }}</UiPre>
      </UiBox>
    </UiBox>
  </UiDisclosure>
</template>
