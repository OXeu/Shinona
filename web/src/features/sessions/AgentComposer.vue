<script setup lang="ts">
import { computed, onUnmounted, ref, useTemplateRef } from 'vue'

import { languageTag, t } from '../../shared/i18n'
import UiDropdown from '../../shared/ui/UiDropdown.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import UiImagePicker from '../../shared/ui/UiImagePicker.vue'
import UiTextarea from '../../shared/ui/UiTextarea.vue'
import { UiBox, UiButton, UiForm, UiImage, UiInline } from '../../shared/ui/elements'
import ComposerConfigOptions from './ComposerConfigOptions.vue'
import ComposerPluginSelector from './ComposerPluginSelector.vue'
import PendingMessages from './PendingMessages.vue'
import { resolveComposerDefaults, runningAction } from './composerPreferences'
import type { ComposerSelection } from './composerPreferences'
import { formatCompactTokens } from './presentation'
import type { AgentSession, ComposerAction, ComposerDefaults, ComposerOptions, ComposerPayload, ComposerRunOptions, ImageAttachment } from './sessionApi'
import { useSessionStore } from './sessionStore'

const props = defineProps<{ session: AgentSession | null; catalog: ComposerOptions; busy: boolean; draft?: boolean; draftLocked?: boolean; savingDefaults: boolean }>()
const emit = defineEmits<{
  send: [action: ComposerAction, payload: ComposerPayload]
  stop: [id: string]
  error: [message: string]
  saveDefaults: [options: ComposerDefaults]
}>()
const prompt = defineModel<string>({ required: true })
const images = defineModel<ImageAttachment[]>('images', { required: true })
const store = useSessionStore()
const sessionId = computed(() => props.session?.id ?? null)
const selection = computed(() => store.composerSelection(sessionId.value))
const lastOptions = computed(() => props.session?.lastOptions)
const mcp = computed({
  get: () => selection.value.mcpServers ?? lastOptions.value?.mcpServers ?? [],
  set: (mcpServers: string[]) => choose({ mcpServers }),
})
const arrowJs = computed({
  get: () => selection.value.arrowJs ?? lastOptions.value?.arrowJs ?? false,
  set: (enabled: boolean) => choose({ arrowJs: enabled }),
})
const reading = ref(false)
const actionMenu = useTemplateRef('actionMenu')
const actionMenuOpen = ref(false)
let disposed = false

const defaults = computed(() => resolveComposerDefaults(props.catalog))
const selectedModel = computed(() => {
  const model = selectedOption('model')
  const { models, modelsLoading } = props.catalog
  return modelsLoading || !models.length || models.some(({ id }) => id === model) ? model : defaults.value.model
})
const permission = computed({
  get: () => selectedOption('permission'),
  set: (permission: string) => choose({ permission }),
})
const thinking = computed({
  get: () => selectedOption('thinking'),
  set: (thinking: string) => choose({ thinking }),
})
const runOptions = computed<ComposerRunOptions>(() => ({
  model: selectedModel.value,
  permission: permission.value,
  thinking: thinking.value,
  mcpServers: permission.value === 'READ_ONLY' ? [] : props.catalog.mcpServers.filter(name => mcp.value.includes(name)),
  arrowJs: arrowJs.value,
}))
defineExpose({ runOptions })

const active = computed(() => props.session?.turns.find((turn) => turn.status === 'RUNNING'))
const queued = computed(() => props.session?.turns.filter((turn) => turn.status === 'QUEUED') ?? [])
const pending = computed(() => {
  const turn = active.value
  const steering = turn?.steering.flatMap((message, index) => message.delivered ? [] : [{
    id: `${turn.id}/steer-${index}`,
    prompt: message.text,
  }]) ?? []
  return [...steering, ...queued.value.map(({ id, prompt, attachments }) => ({ id, prompt, attachments, removable: true }))]
})
const stopTarget = computed(() => active.value ?? queued.value[0])
const hasDraft = computed(() => !!prompt.value.trim() || images.value.length > 0)
const primaryAction = computed<ComposerAction | 'stop'>(() => {
  if (stopTarget.value && !hasDraft.value) return 'stop'
  return props.session?.active ? 'queue' : 'send'
})
const keyboardAction = computed<ComposerAction>(() => {
  if (!props.session?.active) return 'send'
  return active.value && !images.value.length ? runningAction.value : 'queue'
})
const actionMenuAvailable = computed(() => primaryAction.value === 'queue' && canSend('queue'))
const contextUsage = computed(() => {
  const running = active.value
  const model = running ? running.model || props.catalog.defaultModel : selectedModel.value
  const turns = props.session?.turns.filter(turn => (turn.model || props.catalog.defaultModel) === model) ?? []
  const live = running?.liveContext
  return {
    tokens: (live && !live.estimated ? live.tokens : 0) ||
      turns.findLast(turn => turn.contextTokens > 0)?.contextTokens || 0,
    window: props.catalog.models.find(entry => entry.id === model)?.contextWindow || live?.window ||
      turns.findLast(turn => turn.contextWindow > 0)?.contextWindow || 0,
  }
})
const usage = computed(() => {
  const { tokens, window } = contextUsage.value
  if (!tokens) return ''
  const number = new Intl.NumberFormat(languageTag())
  const used = number.format(tokens)
  return window ? t('composer.contextUsage', { used, total: number.format(window) }) : t('composer.contextTokens', { used })
})
const usageValue = computed(() => {
  const { tokens, window } = contextUsage.value
  if (!tokens) return ''
  const used = formatCompactTokens(tokens)
  return window ? `${used}/${formatCompactTokens(window)}` : used
})

function selectedOption(key: keyof ComposerDefaults) {
  const selected = selection.value[key]
  return selected === null ? defaults.value[key] : selected || lastOptions.value?.[key] || defaults.value[key]
}

function choose(patch: ComposerSelection) {
  store.chooseComposerOptions(sessionId.value, patch)
}

function resetOptions() {
  choose({ model: null, permission: null, thinking: null })
}

function label(action: ComposerAction | 'stop') {
  if (action === 'stop') return t('composer.stop')
  if (action === 'steer') return t('composer.steer')
  return t(action === 'queue' ? 'composer.queue' : 'chat.send')
}

function canSend(action: ComposerAction) {
  if ((!props.session && !props.draft) || props.busy || reading.value) return false
  return action === 'steer'
    ? !!active.value && !!prompt.value.trim() && !images.value.length
    : hasDraft.value
}

function send(action: ComposerAction) {
  if (!canSend(action)) return
  emit('send', action, { ...runOptions.value, images: images.value })
}

function stop() {
  if (!props.busy && stopTarget.value) emit('stop', stopTarget.value.id)
}

function submit() {
  const action = primaryAction.value
  if (action === 'stop') stop()
  else send(action)
}

function openActionMenu(event: MouseEvent | KeyboardEvent) {
  if (!actionMenuAvailable.value || !(event.currentTarget instanceof HTMLElement)) return
  if (event instanceof KeyboardEvent && event.key !== 'ContextMenu' && !(event.shiftKey && event.key === 'F10')) return
  event.preventDefault()
  actionMenu.value?.openFrom(event.currentTarget, event instanceof MouseEvent ? event.clientX : undefined)
}

function keydown(event: KeyboardEvent) {
  if (event.key !== 'Enter' || event.shiftKey || event.isComposing) return
  event.preventDefault()
  send(keyboardAction.value)
}

function paste(event: ClipboardEvent) {
  const files = Array.from(event.clipboardData?.files ?? []).filter((file) => file.type.startsWith('image/'))
  if (!files.length) return
  event.preventDefault()
  void attach(files)
}

function drop(event: DragEvent) {
  const files = Array.from(event.dataTransfer?.files ?? [])
  if (files.length) void attach(files)
}

function attachment(file: File): Promise<ImageAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error)
    reader.onload = () => {
      const data = typeof reader.result === 'string' ? reader.result.split(',')[1] : undefined
      if (data) resolve({ name: Array.from(file.name).slice(0, 180).join('') || 'image', mediaType: file.type, data })
      else reject(new Error('invalid image'))
    }
    reader.readAsDataURL(file)
  })
}

async function attach(files: File[]) {
  if (props.busy || reading.value) return
  const total = images.value.reduce((bytes, image) => bytes + Math.floor(image.data.length * 3 / 4) -
    (image.data.endsWith('==') ? 2 : image.data.endsWith('=') ? 1 : 0), 0)
  if (files.length + images.value.length > 4 || total + files.reduce((bytes, file) => bytes + file.size, 0) > 4 * 1024 * 1024 ||
    files.some((file) => !['image/jpeg', 'image/png', 'image/webp'].includes(file.type) || file.size === 0)) {
    emit('error', t('composer.invalidImages'))
    return
  }
  reading.value = true
  try {
    const next = await Promise.all(files.map(attachment))
    if (!disposed) {
      images.value = [...images.value, ...next]
      emit('error', '')
    }
  } catch {
    if (!disposed) emit('error', t('composer.invalidImages'))
  } finally {
    reading.value = false
  }
}

function remove(index: number) {
  images.value = images.value.filter((_, position) => position !== index)
}

onUnmounted(() => { disposed = true })
</script>

<template>
  <PendingMessages :messages="pending" :busy="busy" @remove="emit('stop', $event)" />
  <UiForm
    class="chat-composer"
    @submit.prevent="submit"
    @dragover.prevent
    @drop.prevent="drop"
  >
    <UiBox class="composer-images" :hidden="!images.length">
      <UiBox
        v-for="(image, index) in images"
        :key="index"
        class="composer-image"
      >
        <UiImage :src="'data:' + image.mediaType + ';base64,' + image.data" :alt="image.name" />
        <UiIconButton
          icon="close-line"
          :label="t('composer.removeImage', { name: image.name })"
          :disabled="busy"
          @click="remove(index)"
        />
      </UiBox>
    </UiBox>
    <UiTextarea
      id="agent-prompt"
      v-model="prompt"
      :label="t('chat.prompt')"
      :placeholder="t('chat.placeholder')"
      :disabled="draftLocked"
      rows="2"
      @keydown="keydown"
      @paste="paste"
    />
    <UiBox class="composer-toolbar">
      <UiBox class="composer-tools">
        <UiImagePicker :label="t('composer.attach')" :disabled="busy || reading || images.length >= 4" @files="attach" />
        <ComposerPluginSelector
          v-model="mcp"
          v-model:arrow-js="arrowJs"
          :servers="catalog.mcpServers"
          :mcp-disabled="permission === 'READ_ONLY'"
          :disabled="busy"
        />
        <ComposerConfigOptions
          v-model:permission="permission"
          v-model:thinking="thinking"
          :model-value="selectedModel"
          :models="catalog.models"
          :defaults="catalog.defaults"
          :saving-defaults="savingDefaults"
          @update:model-value="choose({ model: $event })"
          @save-defaults="emit('saveDefaults', $event)"
          @reset="resetOptions"
        />
      </UiBox>
      <UiBox class="composer-submit">
        <UiInline
          class="composer-context"
          :hidden="!usageValue"
          :title="usage"
          :aria-label="usage"
        >
          {{ usageValue }}
        </UiInline>
        <UiBox class="composer-action">
          <UiButton
            type="submit"
            class="composer-send"
            :data-action="primaryAction"
            :title="label(primaryAction)"
            :aria-label="label(primaryAction)"
            :aria-expanded="actionMenuAvailable ? actionMenuOpen : undefined"
            :aria-keyshortcuts="actionMenuAvailable ? 'Shift+F10' : undefined"
            :disabled="primaryAction === 'stop' ? busy : !canSend(primaryAction)"
            @contextmenu="openActionMenu"
            @keydown="openActionMenu"
          >
            <UiIcon :name="primaryAction === 'stop' ? 'stop-circle-line' : primaryAction === 'queue' ? 'list-check-3' : 'arrow-up-line'" />
          </UiButton>
          <UiDropdown
            ref="actionMenu"
            :label="t('composer.otherAction')"
            :hidden="!actionMenuAvailable"
            :disabled="!actionMenuAvailable"
            boundary=".composer-toolbar"
            side="top"
            floating
            @open-change="actionMenuOpen = $event"
          >
            <template #trigger>
              <UiBox class="composer-action-icon">
                <UiIcon name="arrow-up-s-line" />
              </UiBox>
            </template>
            <template #default="{ close }">
              <UiButton class="menu-item" @click="close(true); send('queue')">{{ label('queue') }}</UiButton>
              <UiButton class="menu-item" :disabled="!canSend('steer')" @click="close(true); send('steer')">{{ label('steer') }}</UiButton>
              <UiButton class="menu-item" :disabled="!stopTarget" @click="close(true); stop()">{{ label('stop') }}</UiButton>
            </template>
          </UiDropdown>
        </UiBox>
      </UiBox>
    </UiBox>
  </UiForm>
</template>

<style lang="less" scoped>
.composer-toolbar, .composer-tools, .composer-submit {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px;
}
.composer-toolbar { justify-content: space-between; }
.composer-context {
  display: flex;
  align-items: center;
  gap: 4px;
  min-height: 32px;
  padding: 0 6px;
  color: var(--muted);
  font-size: 0.8125rem;
  border-radius: var(--radius-sm);
}
.composer-context { font-variant-numeric: tabular-nums; white-space: nowrap; }
.composer-action { display: flex; align-items: center; gap: 2px; }
.composer-action-icon {
  display: grid;
  width: 22px;
  height: 34px;
  place-items: center;
  border-radius: var(--radius-full);
  color: var(--muted);
  font-size: 1rem;
}
.composer-action-icon:hover { background: var(--sidebar-bg); color: var(--foreground); }
.composer-send {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  padding: 0;
  border-radius: var(--radius-full);
  font-size: 1.05rem;
}
.composer-send[data-action='stop'] { color: var(--foreground); background: transparent; border-color: var(--border); }
.composer-images { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 6px; }
.composer-image { position: relative; }
.composer-image img { display: block; width: 66px; height: 56px; object-fit: cover; border-radius: var(--radius-md); }
.composer-image button { position: absolute; top: 0; right: 0; min-width: 24px; height: 24px; background: var(--elevated); }
</style>
