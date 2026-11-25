<script setup lang="ts">
import { JSONUIProvider, Renderer } from '@json-render/vue'
import { computed } from 'vue'

import { UiBox, UiInline } from '../../shared/ui/elements'
import { parseDisplaySpec, registry } from '../../shared/ui/jsonRender'

const props = defineProps<{ spec: string; summary: string }>()
const spec = computed(() => parseDisplaySpec(props.spec))
</script>

<template>
  <UiBox class="agent-rich-content">
    <JSONUIProvider :registry="registry">
      <Renderer :spec="spec" :registry="registry" />
    </JSONUIProvider>
    <UiInline :hidden="!!spec">{{ summary }}</UiInline>
  </UiBox>
</template>

<style src="./styles/agent-rich-content.less" lang="less"></style>
