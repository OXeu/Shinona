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
    <div ref="composer" class="chat-overlay" :hidden="!composerVisible">
      <slot name="composer" />
    </div>
    <slot name="after-composer" />
  </div>
</template>
