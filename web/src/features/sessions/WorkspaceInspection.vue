<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, useTemplateRef } from 'vue'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { t } from '../../shared/i18n'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiInline, UiPre, UiSection, UiSubheading } from '../../shared/ui/elements'
import { workspaceApi } from './workspaceApi'
import type { WorkspacePreview } from './workspaceApi'
import { diffLines } from './workspaceInspection'
import type { WorkspaceInspection } from './workspaceInspection'

const props = defineProps<{ sessionId: string; inspection: WorkspaceInspection }>()
const emit = defineEmits<{ back: []; unauthorized: [] }>()
const panel = useTemplateRef<{ $el: HTMLElement }>('panel')
const preview = ref<WorkspacePreview | null>(null)
const loading = ref(false)
const error = ref('')
const lines = computed(() => props.inspection.diff ? diffLines(preview.value?.text ?? '') : [])
let reading: AbortController | undefined

async function refresh() {
  if (loading.value) return
  reading = new AbortController()
  const { signal } = reading
  const { path, diff, revision } = props.inspection
  loading.value = true
  error.value = ''
  try {
    const result = await workspaceApi.preview(props.sessionId, path, diff, revision, signal)
    if (!signal.aborted) preview.value = result
  } catch (failure) {
    if (signal.aborted) return
    if (failure instanceof HttpError && failure.status === 401) emit('unauthorized')
    else error.value = messageFor(failure, 'repository.previewFailed')
  } finally {
    if (!signal.aborted) loading.value = false
  }
}

onMounted(() => {
  panel.value?.$el.focus({ preventScroll: true })
  void refresh()
})
onUnmounted(() => reading?.abort())
</script>

<template>
  <UiSection
    ref="panel"
    class="workspace-view"
    :aria-label="inspection.title"
    :aria-busy="loading"
    tabindex="-1"
    @keydown.esc.stop="emit('back')"
  >
    <UiBox class="workspace-controls">
      <UiIconButton icon="arrow-left-line" :label="t('repository.closePreview')" @click="emit('back')" />
      <UiSubheading class="workspace-ellipsis" :title="inspection.title">{{ inspection.title }}</UiSubheading>
      <UiInline
        v-if="inspection.status"
        class="workspace-status"
        :data-status="inspection.status"
        :title="t('repository.fileStatus', { status: inspection.status })"
      >
        {{ inspection.status }}
      </UiInline>
      <UiIconButton
        icon="refresh-line"
        :label="t('repository.refreshPreview')"
        :aria-disabled="loading"
        @click="refresh"
      />
    </UiBox>
    <UiInline v-if="error || (loading && !preview)" class="workspace-note">{{ error || t('repository.loading') }}</UiInline>
    <UiInline v-if="preview?.truncated" class="workspace-note">{{ t('repository.truncated') }}</UiInline>
    <UiInline v-if="preview && !preview.text" class="workspace-note">{{ t('repository.emptyPreview') }}</UiInline>
    <UiPre v-if="lines.length" class="workspace-code" tabindex="0">
      <UiInline
        v-for="({ line, kind }, index) in lines"
        :key="index"
        class="workspace-diff-line"
        :data-kind="kind"
      >
        {{ line }}
      </UiInline>
    </UiPre>
    <UiPre v-else-if="preview?.text" class="workspace-code" tabindex="0">{{ preview.text }}</UiPre>
  </UiSection>
</template>
