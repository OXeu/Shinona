<script lang="ts">
const bytes = new TextEncoder()
</script>

<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'

import { format as arrowFormat, scriptBytes, scriptFile, styleBytes, styleFile } from '../../../../internal/adapter/pigo/arrow-view.json'
import { UiBox, UiInline } from '../../shared/ui/elements'

const props = defineProps<{ format: string; payload: string; summary: string }>()
const UiArrowView = defineAsyncComponent(() => import('../../shared/ui/UiArrowView.vue'))
// A JSON string may encode each byte in a six-character escape.
const maxPayloadLength = 6 * (scriptBytes + styleBytes) + JSON.stringify({ [scriptFile]: '', [styleFile]: '' }).length + 64
const source = computed(() => {
  if (props.format !== arrowFormat || props.payload.length > maxPayloadLength) return null
  let files: unknown
  try {
    files = JSON.parse(props.payload)
  } catch {
    return null
  }
  if (!files || typeof files !== 'object' || !Object.hasOwn(files, scriptFile)) return null
  const record = files as Record<string, unknown>
  const script = record[scriptFile]
  const css = Object.hasOwn(record, styleFile) ? record[styleFile] : ''
  if (typeof script !== 'string' || !script.trim() || bytes.encode(script).length > scriptBytes ||
      typeof css !== 'string' || bytes.encode(css).length > styleBytes) return null
  return { [scriptFile]: script, [styleFile]: css }
})
</script>

<template>
  <UiBox class="agent-rich-content">
    <UiArrowView
      v-if="source"
      :source="source"
      :summary="summary"
    />
    <UiInline v-else>{{ summary }}</UiInline>
  </UiBox>
</template>

<style src="./styles/agent-rich-content.less" lang="less"></style>
