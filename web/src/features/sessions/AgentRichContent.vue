<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'

import { UiBox, UiInline } from '../../shared/ui/elements'

const props = defineProps<{ format: string; payload: string; summary: string }>()
const UiArrowView = defineAsyncComponent(() => import('../../shared/ui/UiArrowView.vue'))
const source = computed(() => {
  if (props.format !== 'arrow-js' || props.payload.length > 6 * (24 << 10) + 64) return null
  let files: unknown
  try {
    files = JSON.parse(props.payload)
  } catch {
    return null
  }
  if (!files || typeof files !== 'object' || !('main.js' in files)) return null
  const script = files['main.js']
  const css = 'main.css' in files ? files['main.css'] : ''
  const bytes = new TextEncoder()
  if (typeof script !== 'string' || !script.trim() || bytes.encode(script).length > (16 << 10) ||
      typeof css !== 'string' || bytes.encode(css).length > (8 << 10)) return null
  return { 'main.js': script, 'main.css': css }
})
</script>

<template>
  <UiBox class="agent-rich-content">
    <UiArrowView
      v-for="(files, index) in source ? [source] : []"
      :key="payload + index"
      :source="files"
      :summary="summary"
    />
    <UiInline v-for="text in source ? [] : [summary]" :key="text">{{ text }}</UiInline>
  </UiBox>
</template>

<style src="./styles/agent-rich-content.less" lang="less"></style>
