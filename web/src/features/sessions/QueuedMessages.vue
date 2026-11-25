<script setup lang="ts">
import { computed, ref, useId } from 'vue'

import { t } from '../../shared/i18n'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiButton, UiInline, UiList, UiListItem, UiPre, UiSection } from '../../shared/ui/elements'
import type { Turn } from './sessionApi'

const props = defineProps<{ turns: Pick<Turn, 'id' | 'prompt' | 'attachments'>[]; busy: boolean }>()
const emit = defineEmits<{ remove: [id: string] }>()
const listId = useId()
const expanded = ref(false)
const collapsible = computed(() => props.turns.length > 3)
const visibleTurns = computed(() => collapsible.value && !expanded.value ? props.turns.slice(0, 2) : props.turns)
</script>

<template>
  <UiSection class="queued-messages" :hidden="!turns.length" :aria-label="t('composer.queuedMessages')">
    <UiList
      :id="listId"
      class="queued-list"
      role="list"
      tabindex="0"
      :aria-label="t('composer.queuedMessages')"
    >
      <UiListItem
        v-for="(turn, index) in visibleTurns"
        :key="turn.id"
        class="queued-message"
        :aria-posinset="index + 1"
        :aria-setsize="turns.length"
      >
        <UiBox class="queued-bubble">
          <UiPre class="queued-text" :hidden="!turn.prompt">{{ turn.prompt }}</UiPre>
          <UiInline class="queued-attachments" :hidden="!turn.attachments.length">
            {{ t('composer.attached', { images: turn.attachments.join(', ') }) }}
          </UiInline>
        </UiBox>
        <UiIconButton
          class="queued-remove"
          icon="close-line"
          :label="t('composer.removeQueued')"
          :disabled="busy"
          @click="emit('remove', turn.id)"
        />
      </UiListItem>
    </UiList>
    <UiButton
      class="queued-toggle"
      :hidden="!collapsible"
      :aria-expanded="expanded"
      :aria-controls="listId"
      @click="expanded = !expanded"
    >
      {{ expanded ? t('composer.showFewerQueued') : t('composer.showAllQueued', { count: turns.length }) }}
      <UiIcon :name="expanded ? 'arrow-up-s-line' : 'arrow-down-s-line'" />
    </UiButton>
  </UiSection>
</template>

<style src="./styles/queued-messages.less" lang="less" scoped></style>
