<script setup lang="ts">
import { areOptionsEqual, File } from '@pierre/diffs'
import type { FileContents, FileOptions } from '@pierre/diffs'

import { codeViewOptions } from './diffsTheme'
import { useDiffsView } from './useDiffsView'

const props = defineProps<{ name: string; contents: string; plain?: boolean; wrap?: boolean }>()

function options(): FileOptions<undefined, undefined> {
  return {
    ...codeViewOptions,
    disableFileHeader: true,
    overflow: props.wrap ? 'wrap' : 'scroll',
  }
}

function file(): FileContents {
  return { name: props.name, contents: props.contents, lang: props.plain ? 'text' : undefined }
}

const host = useDiffsView(
  () => new File(options()),
  (instance) => {
    const next = options()
    const forceRender = !areOptionsEqual(instance.options, next)
    instance.setOptions(next)
    instance.render({
      file: file(),
      containerWrapper: host.value ?? undefined,
      forceRender,
    })
  },
  [() => props.name, () => props.contents, () => props.plain, () => props.wrap],
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
