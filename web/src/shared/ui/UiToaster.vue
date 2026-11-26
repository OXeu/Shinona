<script setup lang="ts">
import { onMounted, onUnmounted, shallowRef } from 'vue'

import { t } from '../i18n'
import { notificationHost, notificationView } from '../notifications'
import UiIcon from './UiIcon.vue'
import UiIconButton from './UiIconButton.vue'

const state = shallowRef(notificationView.state())
let unsubscribe: (() => void) | undefined

onMounted(() => {
  const sync = () => { state.value = notificationView.state() }
  unsubscribe = notificationView.subscribe(sync)
  sync()
})
onUnmounted(() => unsubscribe?.())
</script>

<template>
  <Teleport :to="notificationHost ?? 'body'">
    <div class="notification-announcement" role="status">{{ state.status }}</div>
    <div class="notification-announcement" role="alert">{{ state.alert }}</div>
    <ol class="notification-list">
      <li
        v-for="item in state.toasts"
        :key="item.id"
        class="notification"
        :data-tone="item.type"
        :data-phase="item.phase"
        @mouseenter="notificationView.pause(item.id)"
        @mouseleave="notificationView.resume(item.id)"
        @focusin="notificationView.pause(item.id)"
        @focusout="notificationView.resume(item.id)"
      >
        <UiIcon :name="item.type === 'error' ? 'error-warning-line' : 'checkbox-circle-line'" />
        <span class="notification-title">{{ item.title }}</span>
        <span v-if="item.count > 1">{{ item.count }}</span>
        <UiIconButton
          icon="close-line"
          :label="t('common.close')"
          @click="notificationView.dismiss(item.id)"
        />
      </li>
    </ol>
  </Teleport>
</template>

<style scoped lang="less">
.notification-announcement {
  position: fixed;
  clip-path: inset(50%);
  width: 1px;
  height: 1px;
  overflow: hidden;
  white-space: nowrap;
}

.notification-list {
  position: fixed;
  z-index: var(--z-toast);
  inset-block-start: 12px;
  inset-inline-end: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  inline-size: min(24rem, calc(100vw - 24px));
  max-block-size: 50vh;
  margin: 0;
  padding: 4px;
  overflow: auto;
  list-style: none;
  pointer-events: none;
}

.notification {
  display: flex;
  align-items: center;
  gap: 8px;
  align-self: flex-end;
  max-inline-size: 100%;
  padding: 6px 8px 6px 12px;
  border: 1px solid color-mix(in srgb, var(--notification-tone) 25%, var(--border));
  border-radius: var(--radius-lg);
  color: var(--foreground);
  background: var(--elevated);
  box-shadow: var(--shadow-md);
  pointer-events: auto;
  transition: opacity 120ms, transform 120ms;
  --notification-tone: var(--success);
}

.notification[data-tone='error'] { --notification-tone: var(--danger); }
.notification > :first-child { color: var(--notification-tone); }
.notification-title { overflow-wrap: anywhere; }
.notification[data-phase='enter'],
.notification[data-phase='closing'],
.notification[data-phase='exit'] {
  opacity: 0;
  transform: translateY(-4px);
}

@media (prefers-reduced-motion: reduce) {
  .notification { transition: none; }
}
</style>
