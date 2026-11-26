<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, useId, useTemplateRef } from 'vue'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import { UiBox, UiButton, UiHeader, UiSection } from '../../shared/ui/elements'
import { useAuthStore } from '../auth/authStore'
import TerminalSurface from './TerminalSurface.vue'
import { terminalApi } from './terminalApi'
import type { TerminalTab } from './terminalApi'
import { activeTerminalTab, rememberTerminalTab } from './terminalPreferences'

const props = defineProps<{ sessionId: string; visible: boolean }>()
const emit = defineEmits<{ empty: []; unauthorized: [] }>()
const auth = useAuthStore()
const request = new AbortController()
const tabList = useTemplateRef<{ $el: HTMLElement }>('tabs')
const surfaces = useTemplateRef<InstanceType<typeof TerminalSurface>[]>('surfaces')
const surface = computed(() => surfaces.value?.find(item => item.terminalId === active.value))
const panelId = useId()
const tabs = ref<TerminalTab[]>([])
const active = ref<string | null>(null)
const visited = ref(new Set<string>())
let settledTab: string | null = null
const mountedTabs = computed(() => tabs.value.filter(({ id }) => visited.value.has(id)))
const loading = ref(true)
const changing = ref(false)

function select(id: string | null) {
  if (settledTab && settledTab !== id) {
    discard(settledTab)
    settledTab = null
  }
  if (id) visited.value.add(id)
  active.value = id
  rememberTerminalTab(auth.user?.id, props.sessionId, id)
}

function discard(id: string) {
  tabs.value = tabs.value.filter(tab => tab.id !== id)
  visited.value.delete(id)
}

function settle(id: string) {
  if (active.value === id) settledTab = id
  else discard(id)
}

async function load() {
  try {
    const entries = await terminalApi.list(props.sessionId, request.signal)
    if (request.signal.aborted) return
    tabs.value = entries
    const saved = activeTerminalTab(auth.user?.id, props.sessionId)
    select(tabs.value.find(({ id }) => id === saved)?.id ?? tabs.value[0]?.id ?? null)
  } catch (error) {
    report(error, 'terminal.unavailable')
    return
  } finally {
    loading.value = false
  }
  if (!tabs.value.length) await add(false)
}

async function add(focus: boolean) {
  if (changing.value || loading.value || request.signal.aborted) return
  changing.value = true
  try {
    const tab = await terminalApi.create(props.sessionId, request.signal)
    if (request.signal.aborted) return
    tabs.value.push(tab)
    select(tab.id)
    if (focus) await focusTab()
  } catch (error) {
    report(error, 'terminal.unavailable')
  } finally {
    changing.value = false
  }
}

async function remove(id: string, index: number) {
  if (changing.value) return
  changing.value = true
  try {
    await terminalApi.close(props.sessionId, id, request.signal)
    if (request.signal.aborted) return
    discard(id)
    if (active.value === id) select(tabs.value[Math.min(index, tabs.value.length - 1)]?.id ?? null)
    if (!tabs.value.length) emit('empty')
    else await focusTab()
  } catch (error) {
    report(error, 'terminal.closeFailed')
  } finally {
    changing.value = false
  }
}

function report(error: unknown, fallback: 'terminal.unavailable' | 'terminal.closeFailed') {
  if (request.signal.aborted) return
  if (error instanceof HttpError && error.status === 401) emit('unauthorized')
  else notify.error(messageFor(error, fallback))
}

async function focusTab() {
  await nextTick()
  if (!props.visible) return
  tabList.value?.$el.querySelector<HTMLElement>('[aria-selected="true"]')?.focus()
}

function move(event: KeyboardEvent, index: number) {
  const last = tabs.value.length - 1
  const next: Record<string, number> = {
    ArrowLeft: index === 0 ? last : index - 1,
    ArrowRight: index === last ? 0 : index + 1,
    Home: 0,
    End: last,
  }
  const tab = tabs.value[next[event.key] ?? -1]
  if (!tab) return
  event.preventDefault()
  select(tab.id)
  void focusTab()
}

onMounted(load)
onUnmounted(() => request.abort())
</script>

<template>
  <UiSection class="terminal-panel" :aria-label="t('terminal.heading')">
    <UiHeader class="terminal-header">
      <UiBox
        ref="tabs"
        class="terminal-tabs"
        role="tablist"
        :aria-label="t('terminal.heading')"
      >
        <UiBox v-for="(tab, index) in tabs" :key="tab.id" class="terminal-tab">
          <UiButton
            :id="panelId + '-' + tab.id"
            role="tab"
            :aria-selected="active === tab.id"
            :aria-controls="visited.has(tab.id) ? panelId + '-panel-' + tab.id : undefined"
            :tabindex="active === tab.id ? 0 : -1"
            @click="select(tab.id)"
            @keydown="move($event, index)"
          >
            {{ t('terminal.tab', { number: tab.number }) }}
          </UiButton>
          <UiIconButton
            class="terminal-tab-close"
            icon="close-line"
            :label="t('terminal.closeTab', { number: tab.number })"
            :disabled="changing"
            @click="remove(tab.id, index)"
          />
        </UiBox>
      </UiBox>
      <UiIconButton
        icon="refresh-line"
        :label="t('terminal.reconnect')"
        :hidden="!surface?.reconnectable"
        @click="surface?.connect()"
      />
      <UiIconButton
        icon="add-line"
        :label="t('terminal.new')"
        :disabled="loading || changing"
        @click="add(true)"
      />
    </UiHeader>
    <UiBox v-if="!tabs.length" class="terminal-empty">
      {{ t(loading || changing ? 'terminal.loading' : 'terminal.noTabs') }}
    </UiBox>
    <TerminalSurface
      v-for="tab in mountedTabs"
      :id="panelId + '-panel-' + tab.id"
      :key="tab.id"
      ref="surfaces"
      role="tabpanel"
      :hidden="active !== tab.id"
      :aria-labelledby="panelId + '-' + tab.id"
      :session-id="sessionId"
      :terminal-id="tab.id"
      @settled="settle(tab.id)"
    />
  </UiSection>
</template>

<style lang="less" scoped>
.terminal-panel {
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.terminal-header {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 2px 6px;
  font-size: 0.8125rem;
}
.terminal-tabs {
  display: flex;
  flex: 1 1 0;
  gap: 4px;
  min-width: 0;
  overflow-x: auto;
  padding: 2px;
}
.terminal-tab {
  display: flex;
  flex: none;
  align-items: center;
  border-radius: var(--radius-sm);
}
.terminal-empty {
  padding: 12px;
  color: var(--muted);
  text-align: center;
}
.terminal-tab:has([aria-selected="true"]) { background: var(--selection-bg); }
.terminal-tab > [role="tab"] {
  padding: 4px 6px;
  color: var(--muted);
  background: transparent;
  white-space: nowrap;
}
.terminal-tab > [aria-selected="true"] { color: var(--foreground); }
.terminal-tab > .terminal-tab-close {
  width: 24px;
  height: 24px;
  min-width: 0;
  padding: 0;
}
</style>
