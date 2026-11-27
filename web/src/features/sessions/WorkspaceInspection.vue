<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, shallowRef } from 'vue'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { t } from '../../shared/i18n'
import UiCodePreview from '../../shared/ui/UiCodePreview.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiInline, UiSection, UiSubheading } from '../../shared/ui/elements'
import { workspaceApi } from './workspaceApi'
import type { WorkspacePreview } from './workspaceApi'
import { workspacePatchFiles } from './workspaceInspection'
import type { WorkspaceInspection } from './workspaceInspection'

const props = defineProps<{ sessionId: string; inspection: WorkspaceInspection }>()
const emit = defineEmits<{ back: []; unauthorized: [] }>()
const panel = shallowRef<HTMLElement | null>(null)
const preview = ref<WorkspacePreview | null>(null)
const loading = ref(false)
const error = ref('')
const wrap = ref(false)
const patchFiles = computed(() => props.inspection.diff && preview.value ? workspacePatchFiles(preview.value.text) : [])
let reading: AbortController | undefined

function setPanel(element: HTMLElement | null) {
  panel.value = element
}

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
  panel.value?.focus({ preventScroll: true })
  void refresh()
})
onUnmounted(() => reading?.abort())
</script>

<template>
  <UiSection
    :element-ref="setPanel"
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
        icon="text-wrap"
        :label="t('repository.wrapLines')"
        :aria-pressed="wrap"
        @click="wrap = !wrap"
      />
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
    <UiBox
      v-if="preview?.text"
      class="workspace-code-scroll"
      tabindex="0"
    >
      <UiCodePreview
        :name="inspection.path || inspection.title"
        :text="preview.text"
        :diffs="patchFiles"
        :plain="inspection.diff && !patchFiles.length"
        :wrap="wrap"
      />
    </UiBox>
  </UiSection>
</template>
