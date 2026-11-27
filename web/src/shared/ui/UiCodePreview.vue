<script setup lang="ts">
import type { FileDiffMetadata } from '@pierre/diffs'

import UiCodeFile from './UiCodeFile.vue'
import UiFileDiff from './UiFileDiff.vue'

const props = defineProps<{
  name: string
  text: string
  diffs: FileDiffMetadata[]
  plain?: boolean
  wrap?: boolean
}>()
</script>

<template>
  <div class="ui-code-preview">
    <UiFileDiff
      v-for="(fileDiff, index) in props.diffs"
      :key="fileDiff.name + ':' + index"
      :file-diff="fileDiff"
      :wrap="wrap"
    />
    <UiCodeFile
      v-if="!props.diffs.length"
      :name="name"
      :contents="text"
      :plain="plain"
      :wrap="wrap"
    />
  </div>
</template>

<style lang="less" scoped>
.ui-code-preview {
  display: block;
  min-width: 0;
}

.ui-code-preview > :deep(.ui-diffs + .ui-diffs) {
  border-top: 1px solid var(--border);
}
</style>
