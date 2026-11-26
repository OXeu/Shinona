<script setup lang="ts">
import { onMounted, onUnmounted, ref, useTemplateRef } from 'vue'

defineProps<{ composerVisible: boolean; centered?: boolean }>()
const composer = useTemplateRef('composer')
const inset = ref(0)
let observer: ResizeObserver | undefined

onMounted(() => {
  if (!composer.value) return
  observer = new ResizeObserver(([entry]) => {
    inset.value = entry ? Math.ceil(entry.target.getBoundingClientRect().height) : 0
  })
  observer.observe(composer.value)
})
onUnmounted(() => observer?.disconnect())
</script>

<template>
  <div class="chat-frame" :data-centered="centered">
    <slot :inset="composerVisible && !centered ? inset + 40 : 0" />
    <slot name="before-composer" />
    <div ref="composer" class="composer-overlay" :hidden="!composerVisible">
      <slot name="composer" />
    </div>
    <slot name="after-composer" />
  </div>
</template>

<style lang="less" scoped>
@import (reference) './styles/breakpoints.less';

.chat-frame {
  position: relative;
  height: 100%;
  min-height: 0;
  background: var(--background);
}

.composer-overlay {
  position: absolute;
  z-index: 1;
  inset: auto 0 0;
  padding: 10px 16px max(16px, env(safe-area-inset-bottom));
  background: linear-gradient(transparent, var(--background) 28%);
}

@media (max-width: @session-compact) {
  .composer-overlay {
    padding: 8px 8px max(8px, env(safe-area-inset-bottom));
  }
}

.chat-frame[data-centered='true'] {
  display: flex;
  flex-direction: column;
  overflow-y: auto;
  gap: 14px;
  padding: clamp(20px, 6vh, 44px) 12px;
}

.chat-frame[data-centered='true'] > :deep(:nth-child(1 of :not([hidden]))) {
  margin-block-start: auto;
}

.chat-frame[data-centered='true'] > :deep(:nth-last-child(1 of :not([hidden]))) {
  margin-block-end: auto;
}

.chat-frame[data-centered='true'] .composer-overlay {
  position: static;
  background: transparent;
  padding: 0;
}
</style>
