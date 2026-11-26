<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, useTemplateRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { languageTag, t } from '../../shared/i18n'
import UiSplitPanel from '../../shared/ui/UiSplitPanel.vue'
import ChattingPanel from './ChattingPanel.vue'
import RevertMessageDialog from './RevertMessageDialog.vue'
import RightSidePanel from './RightSidePanel.vue'
import SessionActionDialog from './SessionActionDialog.vue'
import SessionNavPanel from './SessionNavPanel.vue'
import { sessionTitle } from './presentation'
import { useSessionStore } from './sessionStore'
import { useComposerCatalog } from './useComposerCatalog'
import { useSessionTranscript } from './useSessionTranscript'
import { useTurnActions } from './useTurnActions'
import type { TurnActionError } from './useTurnActions'
import { useWorkbench } from './workbench'

const props = defineProps<{ id?: string }>()
const sessionId = () => props.id
const store = useSessionStore()
const sessionActions = useTemplateRef('sessionActions')
const router = useRouter()
const route = useRoute()
const { compact, sessionsId, sessionsOpen, sessionSelected, workspaceOpen, setHeader } = useWorkbench()

type ChatErrorSource = TurnActionError | 'load' | 'copy' | 'attachment'
const chatErrors = ref<Partial<Record<ChatErrorSource, string>>>({})

function setChatError(source: ChatErrorSource, message: string) {
  if (message) chatErrors.value[source] = message
  else delete chatErrors.value[source]
}

async function missingSession() {
  await router.replace({ name: 'sessions' })
  if (route.name === 'sessions' && !props.id) setChatError('load', t('sessions.notFound'))
}

function returnToLogin() {
  void router.replace({ name: 'login' })
}

const transcript = useSessionTranscript(sessionId, {
  missing: missingSession,
  unauthorized: returnToLogin,
  loadError: (message) => setChatError('load', message),
})
const { session, summary, chatSession, hasOlder, loadingOlder, refreshPaused, resume: resumeSession, updateSummary: updated } = transcript
const { prompt, images, submitting, creating, reverting, revertOpen,
  submit, retryTurn, stop, changePending, askRevert, revertMessage, create } = useTurnActions(sessionId, transcript, setChatError)
const { catalog, savingDefaults, saveComposerDefaults, resume: resumeCatalog } = useComposerCatalog(returnToLogin)
const runningTurnId = computed(() => session.value?.turns.find((turn) => turn.status === 'RUNNING')?.id)
const title = computed(() => {
  const entry = summary.value ?? session.value
  return entry ? sessionTitle(entry) : t('nav.agent')
})

setHeader({ title, creating, create })

const sessionQuery = ref('')
const matchingSessions = computed(() => {
  const query = sessionQuery.value.trim()
  if (!query) return store.recent
  const language = languageTag()
  const terms = query.toLocaleLowerCase(language).split(/\s+/u)
  return store.recent.filter((item) => {
    const project = item.projectId ? store.projectNames.get(item.projectId) : t('projects.default')
    const searchable = [sessionTitle(item), project].join(' ').toLocaleLowerCase(language)
    return terms.every((term) => searchable.includes(term))
  })
})

const sessionSize = ref(22)
const chatSize = ref(60)

watch(sessionId, () => {
  chatErrors.value = {}
})

function loadOlder() {
  if (!reverting.value) void transcript.loadOlder()
}

async function archived(id: string) {
  if (id === props.id) await router.replace({ name: 'sessions' })
}

function resumeRequests() {
  transcript.resumeVisible()
  resumeCatalog()
}

const now = ref(Date.now())
let clock: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  clock = setInterval(() => {
    now.value = Date.now()
  }, 30_000)
  document.addEventListener('visibilitychange', resumeRequests)
  window.addEventListener('online', resumeRequests)
  window.addEventListener('focus', resumeRequests)
  resumeRequests()
})
onUnmounted(() => {
  clearInterval(clock)
  document.removeEventListener('visibilitychange', resumeRequests)
  window.removeEventListener('online', resumeRequests)
  window.removeEventListener('focus', resumeRequests)
})
</script>

<template>
  <UiSplitPanel
    v-model="sessionSize"
    :start-min="216"
    :end-min="workspaceOpen ? 570 : 302"
    :start-collapsed="!sessionsOpen"
    :end-collapsed="compact && sessionsOpen"
    :label="t('sessions.resize')"
  >
    <template #start>
      <SessionNavPanel
        :id="sessionsId"
        v-model="sessionQuery"
        :selected-id="id"
        :sessions="matchingSessions"
        :now="now"
        @select="sessionSelected"
        @action="(action, entry) => sessionActions?.perform(action, entry)"
      />
    </template>
    <template #end>
      <UiSplitPanel
        v-model="chatSize"
        :start-min="302"
        :end-min="250"
        :start-collapsed="compact && workspaceOpen"
        :end-collapsed="!workspaceOpen"
        :label="t('workspace.resizePanel')"
      >
        <template #start>
          <ChattingPanel
            v-model="prompt"
            v-model:images="images"
            :session="chatSession"
            :has-older="hasOlder"
            :loading-older="loadingOlder"
            :refresh-paused="refreshPaused"
            :recent-sessions="matchingSessions"
            :now="now"
            :has-selection="!!id"
            :busy="submitting || creating || reverting"
            :reverting="reverting"
            :catalog="catalog"
            :saving-defaults="savingDefaults"
            :errors="chatErrors"
            @load-older="loadOlder"
            @refresh="resumeSession"
            @submit="submit"
            @stop="stop"
            @pending="changePending"
            @revert="askRevert"
            @retry="retryTurn"
            @save-defaults="saveComposerDefaults"
            @error="setChatError"
            @action="(action, entry) => sessionActions?.perform(action, entry)"
          />
        </template>
        <template #end>
          <RightSidePanel
            :key="id"
            :session-id="id"
            :project-id="session ? session.projectId : summary?.projectId"
            :active="session?.active ?? summary?.active ?? false"
            :running-turn-id="runningTurnId"
            @unauthorized="returnToLogin"
          />
        </template>
      </UiSplitPanel>
    </template>
  </UiSplitPanel>
  <SessionActionDialog ref="sessionActions" @updated="updated" @archived="archived" />
  <RevertMessageDialog v-model="revertOpen" :busy="reverting" @confirm="revertMessage" />
</template>
