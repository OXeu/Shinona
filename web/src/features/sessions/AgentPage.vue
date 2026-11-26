<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted, onUnmounted, ref, useTemplateRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { languageTag, t } from '../../shared/i18n'
import UiSplitPanel from '../../shared/ui/UiSplitPanel.vue'
import { UiBox } from '../../shared/ui/elements'
import breakpoints from '../../shared/ui/styles/breakpoints.module.less'
import { useMediaQuery } from '../../shared/ui/useMediaQuery'
import { useAuthStore } from '../auth/authStore'
import ChattingPanel from './ChattingPanel.vue'
import RevertMessageDialog from './RevertMessageDialog.vue'
import RightSidePanel from './RightSidePanel.vue'
import SessionActionDialog from './SessionActionDialog.vue'
import SessionHeader from './SessionHeader.vue'
import SessionNavPanel from './SessionNavPanel.vue'
import { sessionTitle } from './presentation'
import { useSessionStore } from './sessionStore'
import { rememberTerminalView, terminalView } from './terminalPreferences'
import { useComposerCatalog } from './useComposerCatalog'
import { useSessionTranscript } from './useSessionTranscript'
import { useTurnActions } from './useTurnActions'
import type { TurnActionError } from './useTurnActions'

const TerminalPanel = defineAsyncComponent(() => import('./TerminalPanel.vue'))
const props = defineProps<{ id?: string }>()
const sessionId = () => props.id
const store = useSessionStore()
const auth = useAuthStore()
const sessionActions = useTemplateRef('sessionActions')
const router = useRouter()
const route = useRoute()

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

const sessionSize = ref(25)
const chatSize = ref(70)
const terminalSplit = ref(70)
const terminalOpen = ref(false)
const desktopPanel = ref(true)
const compactPanel = ref(false)
const compactSessions = ref(false)
const compact = useMediaQuery(breakpoints.session)
const showPanel = computed(() => !!props.id && (compact.value ? compactPanel.value : desktopPanel.value))
const showCompactSessions = computed(() => compact.value && compactSessions.value)

watch([() => auth.user?.id, sessionId], ([owner, id]) => {
  terminalOpen.value = terminalView(owner, id).open
}, { immediate: true })

watch(sessionId, () => {
  chatErrors.value = {}
  compactPanel.value = false
  compactSessions.value = false
})

function setTerminalOpen(open: boolean) {
  terminalOpen.value = open
  rememberTerminalView(auth.user?.id, props.id, { open })
}

function togglePanel() {
  if (compact.value) compactPanel.value = !compactPanel.value
  else desktopPanel.value = !desktopPanel.value
}

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
  clock = setInterval(() => { now.value = Date.now() }, 30_000)
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
    class="agent-layout"
    :start-min="238"
    :end-min="430"
    :start-collapsed="compact && !showCompactSessions"
    :end-collapsed="showCompactSessions"
    :label="t('sessions.resize')"
  >
    <template #start>
      <SessionNavPanel
        v-model="sessionQuery"
        :selected-id="id"
        :sessions="matchingSessions"
        :now="now"
        :compact="compact"
        @select="compactSessions = false"
        @close="compactSessions = false"
        @action="(action, entry) => sessionActions?.perform(action, entry)"
      />
    </template>
    <template #end>
      <UiBox class="agent-content">
        <SessionHeader
          :title="title"
          :compact="compact"
          :workspace-available="!!id"
          :panel-open="showPanel"
          :creating="creating"
          :terminal-open="terminalOpen"
          @sessions="compactSessions = true"
          @create="create"
          @toggle-panel="togglePanel"
          @toggle-terminal="setTerminalOpen(!terminalOpen)"
        />
        <UiSplitPanel
          v-model="chatSize"
          class="agent-body"
          :start-min="320"
          :end-min="236"
          :start-collapsed="compact && showPanel"
          :end-collapsed="!showPanel"
          :label="t('workspace.resizePanel')"
        >
          <template #start>
            <UiSplitPanel
              v-model="terminalSplit"
              direction="column"
              :start-min="180"
              :end-min="112"
              :end-collapsed="!terminalOpen"
              :label="t('terminal.resize')"
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
                <TerminalPanel
                  v-if="terminalOpen && id"
                  :key="id"
                  :session-id="id"
                  @close="setTerminalOpen(false)"
                />
              </template>
            </UiSplitPanel>
          </template>
          <template #end>
            <RightSidePanel
              :session-id="session?.id"
              :project-id="session?.projectId"
              :active="!!session?.active"
              :running-turn-id="runningTurnId"
              :visible="showPanel"
              @unauthorized="returnToLogin"
            />
          </template>
        </UiSplitPanel>
      </UiBox>
    </template>
  </UiSplitPanel>
  <SessionActionDialog ref="sessionActions" @updated="updated" @archived="archived" />
  <RevertMessageDialog v-model="revertOpen" :busy="reverting" @confirm="revertMessage" />
</template>
