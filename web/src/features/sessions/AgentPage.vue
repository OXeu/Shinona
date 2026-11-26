<script setup lang="ts">
import { computed, defineAsyncComponent, nextTick, onMounted, onUnmounted, ref, useTemplateRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { languageTag, t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiSplitPanel from '../../shared/ui/UiSplitPanel.vue'
import { UiBox } from '../../shared/ui/elements'
import { useMediaQuery } from '../../shared/ui/useMediaQuery'
import { useAuthStore } from '../auth/authStore'
import ChattingPanel from './ChattingPanel.vue'
import RevertMessageDialog from './RevertMessageDialog.vue'
import RightSidePanel from './RightSidePanel.vue'
import SessionActionDialog from './SessionActionDialog.vue'
import SessionHeader from './SessionHeader.vue'
import SessionNavPanel from './SessionNavPanel.vue'
import { sessionTitle } from './presentation'
import { sessionApi } from './sessionApi'
import type { AgentSession, ComposerAction, ComposerDefaults, ComposerOptions, ComposerPayload, ComposerRunOptions, ImageAttachment, PendingAction, PendingMessage, Policy, SessionSummary, Turn } from './sessionApi'
import { useSessionStore } from './sessionStore'
import { rememberTerminalView, terminalView } from './terminalPreferences'
import { watchLiveSession } from './wire/liveSession'

const TerminalPanel = defineAsyncComponent(() => import('./TerminalPanel.vue'))
const props = defineProps<{ id?: string }>()
const store = useSessionStore()
const auth = useAuthStore()
const sessionActions = useTemplateRef('sessionActions')
const router = useRouter()
const route = useRoute()
const session = ref<AgentSession | null>(null)
const history = ref({ turns: [] as Turn[], nextBeforeSequence: 0 })
const loadingOlder = ref(false)
const chatSession = computed(() => {
  const entry = session.value
  return entry && { ...entry, turns: [...history.value.turns, ...entry.turns] }
})
const policy = ref<Policy | null>(null)
const prompt = ref('')
const images = ref<ImageAttachment[]>([])
const catalog = ref<ComposerOptions>({ defaultModel: '', models: [], modelsLoading: false, mcpServers: [] })
const catalogRequest = new AbortController()
const savingDefaults = ref(false)
let catalogRetry: ReturnType<typeof setTimeout> | undefined
let defaultsVersion = 0
type ChatErrorSource = 'load' | 'submit' | 'stop' | 'pending' | 'revert' | 'retry' | 'create' | 'copy' | 'attachment'
const chatErrors = ref<Partial<Record<ChatErrorSource, string>>>({})
const submitting = ref(false)
const creating = ref(false)
const reverting = ref(false)
const revertOpen = ref(false)
const revertTarget = ref<{ turnId: string; headId: string }>()
const sessionSize = ref(25)
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
const now = ref(Date.now())
let clock: ReturnType<typeof setInterval> | undefined
const chatSize = ref(70)
const terminalSplit = ref(70)
const terminalOpen = ref(false)
watch([() => auth.user?.id, () => props.id], ([owner, id]) => {
  terminalOpen.value = terminalView(owner, id).open
}, { immediate: true })

function setTerminalOpen(open: boolean) {
  terminalOpen.value = open
  rememberTerminalView(auth.user?.id, props.id, { open })
}
const desktopPanel = ref(true)
const compactPanel = ref(false)
const compactSessions = ref(false)
const compact = useMediaQuery('(max-width: 52rem)')
const showPanel = computed(() => !!props.id && (compact.value ? compactPanel.value : desktopPanel.value))
const showCompactSessions = computed(() => compact.value && compactSessions.value)
const runningTurnId = computed(() => session.value?.turns.find((turn) => turn.status === 'RUNNING')?.id)
const title = computed(() => {
  const summary = store.recent.find((item) => item.id === props.id)
  const entry = summary ?? session.value
  return entry ? sessionTitle(entry) : t('nav.agent')
})

interface RequestScope {
  id: string
  controller: AbortController
  timer?: ReturnType<typeof setTimeout>
  fetching: boolean
  live: boolean
  liveRetryAt: number
  revision: number
  reading?: number
}

let current: RequestScope | undefined
let draftSession: SessionSummary | undefined
let draftRequest: AbortController | undefined

function setChatError(source: ChatErrorSource, message: string) {
  if (message) chatErrors.value[source] = message
  else delete chatErrors.value[source]
}

function schedule(scope: RequestScope) {
  clearTimeout(scope.timer)
  if (scope.live || scope.controller.signal.aborted) return
  const delay = session.value?.active || !session.value ? 1_000 : Math.max(1_000, scope.liveRetryAt - Date.now())
  scope.timer = setTimeout(() => { void refresh(scope) }, delay)
}

async function missingSession() {
  await router.replace({ name: 'sessions' })
  if (route.name === 'sessions' && !props.id) setChatError('load', t('sessions.notFound'))
}

function applySession(scope: RequestScope, entry: AgentSession, selectedPolicy: Policy | null) {
  const wasActive = session.value?.active
  const first = entry.turns[0]
  const displayed = chatSession.value?.turns ?? []
  // Reverts truncate the suffix. A matching boundary lets us keep the entire prefix,
  // including completed turns that have moved out of the live page.
  const boundary = first ? displayed.findIndex((turn) => turn.id === first.id) : -1
  const prefix = boundary > 0 ? displayed.slice(0, boundary) : []
  if (prefix.length && prefix.every((turn) => turn.status !== 'QUEUED' && turn.status !== 'RUNNING')) {
    history.value.turns = prefix
  } else {
    history.value = { turns: [], nextBeforeSequence: entry.nextBeforeSequence ?? 0 }
  }
  session.value = entry
  policy.value = selectedPolicy
  setChatError('load', '')
  store.upsert(entry)
  if (!entry.active) {
    if (wasActive && entry.turns.at(-1)?.status === 'SUCCEEDED') notify.success(t('chat.completed'))
    void markRead(scope)
  }
}

async function loadOlder() {
  const scope = current
  const before = history.value.nextBeforeSequence
  const anchor = chatSession.value?.turns[0]?.id
  if (!scope || !before || !anchor || loadingOlder.value || reverting.value) return
  loadingOlder.value = true
  try {
    const result = await sessionApi.olderTurns(scope.id, before, scope.controller.signal)
    if (scope.controller.signal.aborted || chatSession.value?.turns[0]?.id !== anchor) return
    const page = result?.session
    const last = page?.turns.at(-1)
    if (!page || page.id !== scope.id || !last || last.sequence >= before) {
      throw new Error('Invalid older transcript page.')
    }
    history.value = {
      turns: [...page.turns, ...history.value.turns],
      nextBeforeSequence: page.nextBeforeSequence ?? 0,
    }
  } catch {
    if (!scope.controller.signal.aborted && chatSession.value?.turns[0]?.id === anchor) notify.error(t('chat.loadOlderFailed'))
  } finally {
    if (!scope.controller.signal.aborted) loadingOlder.value = false
  }
}

function startLive(scope: RequestScope, entry: AgentSession) {
  if (Date.now() < scope.liveRetryAt) return
  scope.live = true
  const signal = scope.controller.signal
  void watchLiveSession(entry, {
    sync: (updated, settings) => applySession(scope, updated, settings.selected),
    progress: (update) => {
      const turns = session.value?.turns
      const index = turns?.findIndex((turn) => turn.id === update.id) ?? -1
      if (turns && index >= 0) turns[index] = update
    },
    missing: () => { void missingSession() },
  }, signal).catch(() => {
    if (signal.aborted) return
    scope.live = false
    scope.liveRetryAt = Date.now() + 5_000
    schedule(scope)
  })
}

async function markRead(scope: RequestScope) {
  const entry = session.value
  if (!entry?.unread || entry.id !== scope.id || document.visibilityState !== 'visible' || scope.reading !== undefined) return
  const turn = entry.turns.at(-1)
  if (!turn) return
  scope.reading = turn.sequence
  try {
    await sessionApi.markRead(scope.id, turn.sequence, turn.id, scope.controller.signal)
    if (scope.controller.signal.aborted || session.value?.turns.at(-1)?.id !== turn.id) return
    store.markRead(scope.id, turn.sequence)
    if (session.value?.turnCount === turn.sequence && !session.value.active) session.value.unread = false
  } catch {
    // Leave the unread marker intact so opening the session again can retry.
  } finally {
    scope.reading = undefined
  }
}

async function refresh(scope: RequestScope) {
  if (scope.fetching || scope.live || scope.controller.signal.aborted) return
  scope.fetching = true
  const revision = scope.revision
  try {
    const result = await sessionApi.detail(scope.id, scope.controller.signal)
    if (scope.controller.signal.aborted) return
    if (revision !== scope.revision) {
      schedule(scope)
      return
    }
    if (!result.session) {
      await missingSession()
      return
    }
    applySession(scope, result.session, result.sandboxSettings.selected)
    startLive(scope, result.session)
    schedule(scope)
  } catch (error) {
    if (scope.controller.signal.aborted) return
    if (error instanceof HttpError && error.status === 401) {
      await router.replace({ name: 'login' })
      return
    }
    setChatError('load', t('chat.loadRetry'))
    schedule(scope)
  } finally {
    scope.fetching = false
  }
}

function rememberQueued(scope: RequestScope, turn: Turn) {
  const entry = session.value
  if (scope.controller.signal.aborted || entry?.id !== scope.id) return
  scope.revision++
  if (entry.turns.some((item) => item.id === turn.id)) return
  entry.turns.push(turn)
  entry.turnCount = Math.max(entry.turnCount, turn.sequence)
  entry.active = true
  entry.unread = false
  store.upsert(entry)
}

async function submit(action: ComposerAction, payload: ComposerPayload) {
  if (submitting.value || creating.value || reverting.value) return
  setChatError('submit', '')
  const message = prompt.value.trim()
  if ((!message && !payload.images.length) || Array.from(message).length > 8_000) {
    setChatError('submit', t('chat.invalidPrompt'))
    return
  }
  if (!props.id && action === 'send') {
    await submitDraft(message, payload)
    return
  }
  const scope = current
  if (!scope || !session.value) return

  submitting.value = true
  try {
    if (action === 'steer') {
      const running = session.value.turns.find((turn) => turn.status === 'RUNNING')
      if (!running || payload.images.length) {
        setChatError('submit', t('composer.notRunning'))
        return
      }
      await sessionApi.steer(scope.id, running.id, message, scope.controller.signal)
      if (scope.controller.signal.aborted) return
    } else {
      if (!policy.value) {
        setChatError('submit', t('sandbox.none'))
        return
      }
      const submitRun = await sessionApi.submit(scope.id, message, payload, action === 'queue', scope.controller.signal)
      if (scope.controller.signal.aborted || !session.value) return
      rememberQueued(scope, submitRun)
    }
    prompt.value = ''
    images.value = []
    schedule(scope)
  } catch (error) {
    if (!scope.controller.signal.aborted) setChatError('submit', messageFor(error, 'chat.submitFailed'))
  } finally {
    if (!scope.controller.signal.aborted) submitting.value = false
  }
}

async function submitDraft(message: string, payload: ComposerPayload) {
  const projectId = store.selectedProjectId
  const from = route.fullPath
  draftRequest = new AbortController()
  const { signal } = draftRequest
  submitting.value = true
  try {
    if (!draftSession || draftSession.projectId !== (projectId || null)) {
      const createSession = await sessionApi.create(projectId, signal)
      if (signal.aborted || route.fullPath !== from) return
      draftSession = createSession
      store.upsert(createSession)
    }
    const entry = draftSession
    const submitRun = await sessionApi.submit(entry.id, message, payload, false, signal)
    if (signal.aborted || route.fullPath !== from) return
    store.moveDraftComposerOptions(entry.id)
    store.upsert({ ...entry, turnCount: submitRun.sequence, active: true })
    await router.push({ name: 'session', params: { id: entry.id } })
  } catch (error) {
    if (!signal.aborted && route.fullPath === from) setChatError('submit', messageFor(error, 'chat.submitFailed'))
  } finally {
    if (!signal.aborted) submitting.value = false
  }
}

async function retryTurn(id: string, options: ComposerRunOptions) {
  const scope = current
  if (!scope || session.value?.active || submitting.value || creating.value || reverting.value) return
  setChatError('retry', '')
  submitting.value = true
  try {
    const result = await sessionApi.retry(scope.id, id, options, scope.controller.signal)
    if (scope.controller.signal.aborted) return
    rememberQueued(scope, result)
    schedule(scope)
  } catch (error) {
    if (!scope.controller.signal.aborted) setChatError('retry', messageFor(error, 'chat.retryFailed'))
  } finally {
    if (!scope.controller.signal.aborted) submitting.value = false
  }
}

async function stop(id: string) {
  const scope = current
  if (!scope || submitting.value || reverting.value) return
  setChatError('stop', '')
  submitting.value = true
  try {
    await sessionApi.stop(scope.id, id, scope.controller.signal)
    if (scope.controller.signal.aborted) return
    notify.success(t('composer.stopRequested'))
    schedule(scope)
  } catch (error) {
    if (!scope.controller.signal.aborted) setChatError('stop', messageFor(error, 'composer.stopFailed'))
  } finally {
    if (!scope.controller.signal.aborted) submitting.value = false
  }
}

async function changePending(message: PendingMessage, action: PendingAction, runId: string | undefined, options: ComposerRunOptions) {
  const scope = current
  if (!scope || submitting.value || creating.value || reverting.value) return
  if (action === 'move' && message.kind === 'queue' && (!runId || message.attachments.length)) return
  const signal = scope.controller.signal
  setChatError('pending', '')
  submitting.value = true
  try {
    if (action === 'move') {
      if (message.kind === 'steering') {
        await sessionApi.queueSteering(scope.id, message.runId, message.id, options, signal)
      } else if (runId) {
        await sessionApi.steerQueued(scope.id, message.id, runId, signal)
      }
    } else {
      const draft = await sessionApi.takePending(scope.id, message, signal)
      if (action === 'draft' && !signal.aborted) {
        prompt.value = [prompt.value, draft.prompt].filter(Boolean).join('\n\n')
        images.value = [...images.value, ...draft.images]
      }
    }
    if (!signal.aborted) scope.revision++
  } catch (error) {
    if (!signal.aborted) setChatError('pending', messageFor(error, 'composer.pendingActionFailed'))
  } finally {
    if (!signal.aborted) {
      submitting.value = false
      schedule(scope)
      void refresh(scope)
    }
  }
}

function askRevert(turnId: string) {
  const entry = session.value
  const head = entry?.turns.at(-1)
  if (!head || entry?.active || submitting.value || creating.value || reverting.value) return
  revertTarget.value = { turnId, headId: head.id }
  revertOpen.value = true
}

async function revertMessage() {
  const scope = current
  const target = revertTarget.value
  const entry = session.value
  if (!scope || !target || !entry || submitting.value || reverting.value) return
  setChatError('revert', '')
  reverting.value = true
  try {
    const revertTurn = await sessionApi.revert(scope.id, target.turnId, target.headId, scope.controller.signal)
    if (scope.controller.signal.aborted) return
    scope.revision++
    const summary = revertTurn.session
    if (session.value === entry) {
      session.value = { ...summary, turns: entry.turns.filter((turn) => turn.sequence <= summary.turnCount), lastOptions: entry.lastOptions }
      history.value.turns = history.value.turns.filter((turn) => turn.sequence <= summary.turnCount)
      store.upsert(summary)
    }
    prompt.value = revertTurn.prompt
    images.value = revertTurn.images
    notify.success(t('chat.reverted'))
  } catch (error) {
    if (!scope.controller.signal.aborted) setChatError('revert', messageFor(error, 'chat.revertFailed'))
  } finally {
    if (!scope.controller.signal.aborted) {
      revertOpen.value = false
      reverting.value = false
      revertTarget.value = undefined
      if (scope.fetching) schedule(scope)
      else void refresh(scope)
      await nextTick()
      if (!scope.controller.signal.aborted) document.getElementById('agent-prompt')?.focus()
    }
  }
}

async function create() {
  if (creating.value || submitting.value) return
  const from = route.fullPath
  setChatError('create', '')
  creating.value = true
  try {
    const createSession = await sessionApi.create(store.selectedProjectId)
    if (route.fullPath !== from) return
    store.upsert(createSession)
    await router.push({ name: 'session', params: { id: createSession.id } })
  } catch (error) {
    if (route.fullPath === from) setChatError('create', messageFor(error, 'sessions.createFailed'))
  } finally {
    creating.value = false
  }
}

function updated(item: SessionSummary) {
  if (session.value?.id === item.id && current) {
    current.revision++
    session.value.title = item.title
    session.value.defaultTitle = item.defaultTitle
    session.value.pinned = item.pinned
  }
}

async function archived(id: string) {
  if (id === props.id) await router.replace({ name: 'sessions' })
}

function togglePanel() {
  if (compact.value) compactPanel.value = !compactPanel.value
  else desktopPanel.value = !desktopPanel.value
}

watch(() => props.id, (id, _, onCleanup) => {
  draftRequest?.abort()
  draftSession = undefined
  current = undefined
  chatErrors.value = {}
  history.value = { turns: [], nextBeforeSequence: 0 }
  loadingOlder.value = false
  session.value = null
  policy.value = null
  prompt.value = ''
  images.value = []
  submitting.value = false
  reverting.value = false
  revertOpen.value = false
  revertTarget.value = undefined
  compactPanel.value = false
  compactSessions.value = false
  if (!id) return
  const scope: RequestScope = { id, controller: new AbortController(), fetching: false, live: false, liveRetryAt: 0, revision: 0 }
  current = scope
  onCleanup(() => {
    scope.controller.abort()
    clearTimeout(scope.timer)
  })
  void refresh(scope)
}, { immediate: true })

watch(() => store.recent.find((item) => item.id === props.id)?.updatedAt, (newTime) => {
  if (current && session.value && newTime && session.value.updatedAt !== newTime && !session.value.active) void refresh(current)
})

watch(() => store.recent.find((item) => item.id === props.id)?.projectId, (projectId) => {
  const entry = session.value
  if (entry && entry.id === props.id && projectId !== undefined) entry.projectId = projectId
})

function markVisible() {
  if (current) void markRead(current)
}

async function saveComposerDefaults(input: ComposerDefaults) {
  if (savingDefaults.value) return
  savingDefaults.value = true
  const signal = catalogRequest.signal
  try {
    const setComposerDefaults = await sessionApi.saveComposerDefaults(input, signal)
    if (signal.aborted) return
    defaultsVersion++
    catalog.value.defaults = setComposerDefaults
    notify.success(t('composer.defaultsSaved'))
  } catch (error) {
    if (!signal.aborted) notify.error(messageFor(error, 'composer.defaultsFailed'))
  } finally {
    savingDefaults.value = false
  }
}

async function loadComposerOptions(attempt = 0) {
  const { signal } = catalogRequest
  const version = defaultsVersion
  try {
    const composerOptions = await sessionApi.composerOptions(signal)
    if (signal.aborted) return
    if (defaultsVersion !== version) composerOptions.defaults = catalog.value.defaults
    catalog.value = composerOptions
    if (composerOptions.modelsLoading) catalogRetry = setTimeout(loadComposerOptions, 1_000)
  } catch (error) {
    if (signal.aborted) return
    if (error instanceof HttpError && error.status === 401) {
      await router.replace({ name: 'login' })
    } else if (attempt < 2) {
      catalogRetry = setTimeout(() => { void loadComposerOptions(attempt + 1) }, 1_000)
    } else {
      notify.error(messageFor(error, 'composer.optionsFailed'))
    }
  }
}

onMounted(() => {
  clock = setInterval(() => { now.value = Date.now() }, 30_000)
  document.addEventListener('visibilitychange', markVisible)
  void loadComposerOptions()
})
onUnmounted(() => {
  clearInterval(clock)
  document.removeEventListener('visibilitychange', markVisible)
  catalogRequest.abort()
  clearTimeout(catalogRetry)
  draftRequest?.abort()
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
                  :has-older="history.nextBeforeSequence > 0"
                  :loading-older="loadingOlder"
                  :recent-sessions="matchingSessions"
                  :now="now"
                  :has-selection="!!id"
                  :busy="submitting || creating || reverting"
                  :reverting="reverting"
                  :catalog="catalog"
                  :saving-defaults="savingDefaults"
                  :errors="chatErrors"
                  @load-older="loadOlder"
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
              :session-id="id"
              :project-id="session?.projectId"
              :active="!!session?.active"
              :running-turn-id="runningTurnId"
              :visible="showPanel"
            />
          </template>
        </UiSplitPanel>
      </UiBox>
    </template>
  </UiSplitPanel>
  <SessionActionDialog ref="sessionActions" @updated="updated" @archived="archived" />
  <RevertMessageDialog v-model="revertOpen" :busy="reverting" @confirm="revertMessage" />
</template>
