<script setup lang="ts">
import { t } from '../../shared/i18n'
import UiIcon from '../../shared/ui/UiIcon.vue'
import { UiBox, UiButton, UiPre } from '../../shared/ui/elements'

defineProps<{ text: string; tone?: 'error'; retryable?: boolean; retryDisabled?: boolean; showDetails?: boolean }>()
const emit = defineEmits<{ retry: []; details: [] }>()
</script>

<template>
  <UiBox class="chat-system-message" :data-tone="tone">
    <UiIcon name="error-warning-line" :hidden="tone !== 'error'" />
    <UiPre>{{ text }}</UiPre>
    <UiBox class="system-message-actions" :hidden="!retryable && !showDetails">
      <UiButton
        class="system-message-action"
        :hidden="!retryable"
        :disabled="retryDisabled"
        @click="emit('retry')"
      >
        {{ t('chat.retryTurn') }}
      </UiButton>
      <UiButton class="system-message-action" :hidden="!showDetails" @click="emit('details')">
        {{ t('chat.failureDetails') }}
      </UiButton>
    </UiBox>
  </UiBox>
</template>

<style lang="less" scoped>
.chat-system-message {
  display: flex;
  align-items: baseline;
  gap: 6px;
  min-width: 0;
  padding: 3px 10px;
  border-radius: var(--radius-full);
  background: var(--sidebar-bg);
  color: var(--muted);
}

.chat-system-message[data-tone='error'] {
  border: 1px solid color-mix(in srgb, var(--danger) 25%, var(--border));
  border-radius: var(--radius-sm);
  color: var(--danger);
  background: color-mix(in srgb, var(--danger) 6%, var(--background));
}

.chat-system-message pre { font-size: 0.77rem; }

.system-message-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.system-message-action {
  min-height: 0;
  padding: 0 3px;
  border: 0;
  color: inherit;
  background: transparent;
  font: inherit;
  font-size: 0.77rem;
  text-decoration: underline;
  white-space: nowrap;
}
</style>
