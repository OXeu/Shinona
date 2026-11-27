<script setup lang="ts">
import { areOptionsEqual, FileDiff } from '@pierre/diffs'
import type { FileDiffMetadata, FileDiffOptions } from '@pierre/diffs'

import { codeViewOptions } from './diffsTheme'
import { useDiffsView } from './useDiffsView'

const props = defineProps<{ fileDiff: FileDiffMetadata; wrap?: boolean }>()

function options(): FileDiffOptions<undefined, undefined> {
  return {
    ...codeViewOptions,
    diffStyle: 'unified',
    hunkSeparators: 'line-info',
    overflow: props.wrap ? 'wrap' : 'scroll',
  }
}

const host = useDiffsView(
  () => new FileDiff(options()),
  (instance) => {
    const next = options()
    const forceRender = !areOptionsEqual(instance.options, next)
    instance.setOptions(next)
    instance.render({
      fileDiff: props.fileDiff,
      containerWrapper: host.value ?? undefined,
      forceRender,
    })
  },
  [() => props.fileDiff, () => props.wrap],
)
</script>

<template>
  <div ref="host" class="ui-diffs" />
</template>

<style lang="less" scoped>
.ui-diffs {
  display: block;
  min-width: 0;
}
</style>
