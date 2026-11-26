<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, useId, useTemplateRef, watch } from 'vue'
import { useRouter } from 'vue-router'

import { HttpError } from '../../shared/http'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { RetryBackoff } from '../../shared/retryBackoff'
import UiIconButton from '../../shared/ui/UiIconButton.vue'
import UiSearchField from '../../shared/ui/UiSearchField.vue'
import UiSplitPanel from '../../shared/ui/UiSplitPanel.vue'
import { UiBox, UiButton, UiHeader, UiNav } from '../../shared/ui/elements'
import SessionList from './SessionList.vue'
import SessionProjectPicker from './SessionProjectPicker.vue'
import type { SessionAction } from './presentation'
import type { SessionSummary } from './sessionApi'
import { useSessionStore } from './sessionStore'
import { sessionPollInterval } from './sessionListWorkerMessages'

const props = defineProps<{
  selectedId?: string
  sessions: SessionSummary[]
  now: number
  compact?: boolean
}>()
const emit = defineEmits<{ select: []; close: []; action: [action: SessionAction, entry: SessionSummary] }>()
const store = useSessionStore()
const router = useRouter()
const query = defineModel<string>({ required: true })
const searchOpen = ref(false)
const searchId = useId()
const searchField = useTemplateRef('searchField')
let searchTrigger: EventTarget | null = null
const recentOpen = ref(false)
const topSize = ref(60)
const ordered = computed(() => props.sessions
  .toSorted((a, b) => Number(b.pinned) - Number(a.pinned) || b.updatedAt.localeCompare(a.updatedAt)))
const needsAttention = computed(() => ordered.value.filter((item) => !item.active && (item.unread || item.turnCount === 0)))
const active = computed(() => ordered.value.filter((item) => item.active))
const recent = computed(() => ordered.value.filter((item) => !item.active && !item.unread && item.turnCount > 0))
const visibleRecent = computed(() => recentOpen.value || query.value.trim() ? recent.value : recent.value.slice(0, 5))

let timer: ReturnType<typeof setTimeout> | undefined
let listing: AbortController | undefined
let hadError = false
const retry = new RetryBackoff()
const refreshPaused = ref(false)

async function openSearch(event: MouseEvent) {
  searchTrigger = event.currentTarget
  searchOpen.value = true
  await nextTick()
  searchField.value?.focus()
}

async function closeSearch() {
  searchOpen.value = false
  query.value = ''
  await nextTick()
  if (searchTrigger instanceof HTMLElement) searchTrigger.focus({ preventScroll: true })
}

async function refresh() {
  clearTimeout(timer)
  listing?.abort()
  if (document.hidden) return
  listing = new AbortController()
  const { signal } = listing
  let delay: number | undefined
  try {
    await store.refresh(signal)
    if (!signal.aborted) {
      hadError = false
      retry.reset()
      delay = sessionPollInterval
    }
  } catch (error) {
    if (signal.aborted) return
    if (error instanceof HttpError && error.status === 401) {
      await router.replace({ name: 'login' })
      return
    }
    if (!hadError) notify.error(t('sessions.loadFailed'))
    hadError = true
    delay = retry.next()
    refreshPaused.value = delay === undefined
  } finally {
    if (!signal.aborted && !document.hidden && delay !== undefined) {
      timer = setTimeout(() => {
        void refresh()
      }, delay)
    }
  }
}

function restart() {
  retry.reset()
  refreshPaused.value = false
  void refresh()
}

function resumeOnFocus() {
  if (refreshPaused.value) restart()
}

watch(() => store.selectedProjectId, restart)
onMounted(() => {
  document.addEventListener('visibilitychange', restart)
  window.addEventListener('online', restart)
  window.addEventListener('focus', resumeOnFocus)
  restart()
})
onUnmounted(() => {
  document.removeEventListener('visibilitychange', restart)
  window.removeEventListener('online', restart)
  window.removeEventListener('focus', resumeOnFocus)
  listing?.abort()
  clearTimeout(timer)
})
</script>

<template>
  <UiNav class="session-nav" :aria-label="t('sessions.heading')">
    <UiHeader class="session-nav-header">
      <UiBox class="session-nav-controls" :inert="searchOpen">
        <SessionProjectPicker compact />
        <UiBox class="session-nav-actions">
          <UiIconButton
            v-if="refreshPaused"
            icon="refresh-line"
            :label="t('sessions.refresh')"
            @click="restart"
          />
          <UiIconButton
            icon="close-line"
            :label="t('sessions.hide')"
            :hidden="!compact"
            @click="emit('close')"
          />
          <UiIconButton
            icon="search-line"
            :label="t('sessions.search')"
            :aria-expanded="searchOpen"
            :aria-controls="searchId"
            @click="openSearch"
          />
        </UiBox>
      </UiBox>
      <UiBox
        :id="searchId"
        class="session-nav-search"
        :data-open="searchOpen"
        :inert="!searchOpen"
        @keydown.esc.stop.prevent="closeSearch"
      >
        <UiSearchField ref="searchField" v-model="query" :label="t('sessions.search')" />
        <UiIconButton
          class="session-nav-search-close"
          icon="close-line"
          :label="t('common.close')"
          @click="closeSearch"
        />
      </UiBox>
    </UiHeader>

    <UiSplitPanel
      v-model="topSize"
      class="session-groups"
      direction="column"
      :start-min="90"
      :end-min="72"
      :label="t('sessions.resizeRecent')"
    >
      <template #start>
        <UiBox class="session-scroll">
          <SessionList
            :label="t('sessions.needsAttention')"
            :entries="needsAttention"
            :now="now"
            :selected-id="selectedId"
            @select="emit('select')"
            @action="(action, entry) => emit('action', action, entry)"
          />
          <SessionList
            :label="t('sessions.active')"
            :entries="active"
            :now="now"
            :selected-id="selectedId"
            @select="emit('select')"
            @action="(action, entry) => emit('action', action, entry)"
          />
        </UiBox>
      </template>
      <template #end>
        <UiBox class="session-scroll">
          <SessionList
            :label="t('sessions.recent')"
            :entries="visibleRecent"
            :now="now"
            :selected-id="selectedId"
            @select="emit('select')"
            @action="(action, entry) => emit('action', action, entry)"
          />
          <UiButton
            class="recent-toggle"
            :hidden="recent.length <= 5 || !!query.trim()"
            :aria-expanded="recentOpen"
            @click="recentOpen = !recentOpen"
          >
            {{ recentOpen ? t('sessions.showFewer') : t('sessions.showAll', { count: recent.length }) }}
          </UiButton>
        </UiBox>
      </template>
    </UiSplitPanel>
  </UiNav>
</template>
