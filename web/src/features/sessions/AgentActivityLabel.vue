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
.activity-label {
  --activity-highlight: color-mix(in srgb, var(--muted), var(--background) 10%);
}

.activity-letter {
  white-space: pre;
  --letter-delay: 0ms;
  animation: activity-letter-lighten 2s ease-in-out var(--letter-delay) infinite;
}

each(range(32), {
  .activity-letter:nth-child(@{value}) {
    @delay: (@value - 1) * 40ms;
    --letter-delay: @delay;
  }
});

@keyframes activity-letter-lighten {
  0%, 100% { color: var(--muted); }
  50% { color: var(--activity-highlight); }
}

@media (prefers-color-scheme: dark) {
  .activity-label { --activity-highlight: var(--foreground); }
}

@media (prefers-reduced-motion: reduce) {
  .activity-letter { animation: none; }
}
</style>
