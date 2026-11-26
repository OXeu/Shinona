<!--
From Uiverse.io by dexter-st - https://uiverse.io/dexter-st/young-newt-20
MIT License, Copyright (c) dexter-st
-->
<script setup lang="ts">
import { computed } from 'vue'

import { UiInline } from '../../shared/ui/elements'

const props = defineProps<{ text: string }>()
const segmenter = new Intl.Segmenter()
const letters = computed(() => Array.from(segmenter.segment(props.text), ({ segment }) => segment))
</script>

<template>
  <UiInline class="activity-label" aria-hidden="true">
    <UiInline
      v-for="(letter, index) in letters"
      :key="index"
      class="activity-letter"
    >
      {{ letter }}
    </UiInline>
  </UiInline>
</template>

<style scoped lang="less">
.activity-letter {
  display: inline-block;
  white-space: pre;
  --letter-delay: 0ms;
  animation:
    activity-letter-enter 420ms ease-in-out var(--letter-delay) both,
    activity-letter-glow 2s ease-in-out calc(420ms + var(--letter-delay)) infinite;
}

each(range(32), {
  .activity-letter:nth-child(@{value}) {
    @delay: (@value - 1) * 40ms;
    --letter-delay: @delay;
  }
});

@keyframes activity-letter-enter {
  0% { opacity: 0.45; filter: blur(0.16em); }
  45% { transform: scale(1.35); filter: blur(0.24em); }
  100% { opacity: 1; transform: scale(1); filter: none; }
}

@keyframes activity-letter-glow {
  50% {
    color: var(--foreground);
    text-shadow: 0 0 3px color-mix(in srgb, currentColor 28%, transparent);
  }
}

@media (prefers-reduced-motion: reduce) {
  .activity-letter { animation: none; }
}
</style>
