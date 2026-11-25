<script setup lang="ts">
import { computed, ref, useId } from 'vue'

import { t } from '../../shared/i18n'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiButton, UiInline, UiList, UiListItem, UiPre, UiSection } from '../../shared/ui/elements'

interface PendingMessage {
  id: string
  prompt: string
  attachments?: string[]
  removable?: boolean
}

const props = defineProps<{ messages: PendingMessage[]; busy: boolean }>()
const emit = defineEmits<{ remove: [id: string] }>()
const listId = useId()
const expanded = ref(false)
const collapsible = computed(() => props.messages.length > 3)
const visibleMessages = computed(() => collapsible.value && !expanded.value ? props.messages.slice(0, 2) : props.messages)
</script>

<template>
  <UiSection class="pending-messages" :hidden="!messages.length" :aria-label="t('composer.pendingMessages')">
    <UiList
      :id="listId"
      class="pending-list"
      role="list"
      tabindex="0"
      :aria-label="t('composer.pendingMessages')"
    >
      <UiListItem
        v-for="(message, index) in visibleMessages"
        :key="message.id"
        class="pending-message"
        :aria-posinset="index + 1"
        :aria-setsize="messages.length"
      >
        <UiBox class="pending-bubble">
          <UiPre class="pending-text" :hidden="!message.prompt">{{ message.prompt }}</UiPre>
          <UiInline class="pending-attachments" :hidden="!message.attachments?.length">
            {{ t('composer.attached', { images: message.attachments?.join(', ') ?? '' }) }}
          </UiInline>
        </UiBox>
        <UiIconButton
          class="pending-remove"
          icon="close-line"
          :label="t('composer.removeQueued')"
          :hidden="!message.removable"
          :disabled="busy"
          @click="emit('remove', message.id)"
        />
      </UiListItem>
    </UiList>
    <UiButton
      class="pending-toggle"
      :hidden="!collapsible"
      :aria-expanded="expanded"
      :aria-controls="listId"
      @click="expanded = !expanded"
    >
      {{ expanded ? t('composer.showFewerPending') : t('composer.showAllPending', { count: messages.length }) }}
      <UiIcon :name="expanded ? 'arrow-up-s-line' : 'arrow-down-s-line'" />
    </UiButton>
  </UiSection>
</template>

<style src="./styles/pending-messages.less" lang="less" scoped></style>
