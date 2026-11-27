import { computed, ref, watch } from 'vue'

import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { sessionApi } from './sessionApi'
import type { AgentSession, Policy, SessionSummary, Turn } from './sessionApi'
import { useSessionStore } from './sessionStore'
import { SessionUpdates } from './sessionUpdates'

interface TranscriptNavigation {
  missing(): Promise<void>
  unauthorized(): void
  loadError(message: string): void
}

export function useSessionTranscript(sessionId: () => string | undefined, navigation: TranscriptNavigation) {
  const store = useSessionStore()
  const session = ref<AgentSession | null>(null)
  const summary = computed(() => store.recent.find((item) => item.id === sessionId()))
  const policy = ref<Policy | null>(null)
  const history = ref({ turns: [] as Turn[], nextBeforeSequence: 0 })
  const hasOlder = computed(() => history.value.nextBeforeSequence > 0)
  const loadingOlder = ref(false)
  const refreshPaused = ref(false)
  const pageTurns = computed(() => [...history.value.turns, ...(session.value?.turns ?? [])])
  const runningTurn = computed(() => {
    const earlier = session.value?.earlierTurn
    return earlier?.status === 'RUNNING' ? earlier : session.value?.turns.find((turn) => turn.status === 'RUNNING')
  })
  const chatSession = computed(() => {
    const entry = session.value
    if (!entry) return null
    const earlier = entry.earlierTurn
    if (!earlier) return { ...entry, turns: pageTurns.value }
    const turns = [...pageTurns.value]
    const found = turns.findIndex((turn) => turn.sequence >= earlier.sequence)
    const index = found < 0 ? turns.length : found
    turns.splice(index, turns[index]?.id === earlier.id ? 1 : 0, earlier)
    return { ...entry, turns }
  })
  let current: SessionUpdates | undefined
  let reading: SessionUpdates | undefined

  function applySession(scope: SessionUpdates, entry: AgentSession, selectedPolicy: Policy | null) {
    const wasActive = session.value?.active
    const previousRunning = runningTurn.value?.id
    const first = entry.turns[0]
    const displayed = pageTurns.value
    // Keep known completed turns as the live page advances. A revert can
    // keep the same prefix while dropping its suffix.
    const boundary = first ? displayed.findIndex((turn) => turn.id === first.id) : -1
    const earlier = entry.earlierTurn
    const prefix = boundary > 0 ? displayed.slice(0, boundary) : []
    if (prefix.length && prefix.every((turn) => earlier?.id === turn.id || (turn.status !== 'QUEUED' && turn.status !== 'RUNNING'))) {
      history.value.turns = prefix.map((turn) => earlier?.id === turn.id ? earlier : turn)
    } else {
      history.value = { turns: [], nextBeforeSequence: entry.nextBeforeSequence ?? 0 }
    }
    session.value = entry
    policy.value = selectedPolicy
    navigation.loadError('')
    refreshPaused.value = false
    store.upsert(entry)
    if (!entry.active) {
      const finishedEarlier = earlier?.id === previousRunning && earlier?.status === 'SUCCEEDED'
      if (wasActive && (entry.turns.at(-1)?.status === 'SUCCEEDED' || finishedEarlier)) {
        notify.success(t('chat.completed'))
      }
      void markRead(scope)
    }
  }

  async function loadOlder() {
    const scope = current
    const before = history.value.nextBeforeSequence
    const anchor = pageTurns.value[0]?.id
    if (!scope || !before || !anchor || loadingOlder.value) return
    loadingOlder.value = true
    try {
      const result = await sessionApi.olderTurns(scope.id, before, scope.signal)
      if (scope.signal.aborted || pageTurns.value[0]?.id !== anchor) return
      const page = result.session
      const last = page?.turns.at(-1)
      if (!page || page.id !== scope.id || !last || last.sequence >= before) {
        throw new Error('Invalid older transcript page.')
      }
      history.value = {
        turns: [...page.turns, ...history.value.turns],
        nextBeforeSequence: page.nextBeforeSequence ?? 0,
      }
    } catch {
      if (!scope.signal.aborted && pageTurns.value[0]?.id === anchor) notify.error(t('chat.loadOlderFailed'))
    } finally {
      if (!scope.signal.aborted) loadingOlder.value = false
    }
  }

  async function markRead(scope: SessionUpdates) {
    const entry = session.value
    if (scope.signal.aborted) return
    if (!entry?.unread || entry.id !== scope.id || document.visibilityState !== 'visible' || reading === scope) return
    const turn = entry.turns.at(-1)
    if (!turn) return
    reading = scope
    try {
      await sessionApi.markRead(scope.id, turn.sequence, turn.id, scope.signal)
      if (scope.signal.aborted || session.value?.turns.at(-1)?.id !== turn.id) return
      store.markRead(scope.id, turn.sequence)
      if (session.value?.turnCount === turn.sequence && !session.value.active) session.value.unread = false
    } catch {
      // Keep the unread marker so opening or focusing the session can retry.
    } finally {
      if (reading === scope) {
        reading = undefined
        // A newer turn may have arrived while this request was in flight.
        if (session.value?.turns.at(-1)?.id !== turn.id) void markRead(scope)
      }
    }
  }

  function rememberQueued(scope: SessionUpdates, turn: Turn) {
    const entry = session.value
    if (scope.signal.aborted || entry?.id !== scope.id) return
    scope.invalidate()
    if (entry.turns.some((item) => item.id === turn.id)) return
    entry.turns.push(turn)
    entry.turnCount = Math.max(entry.turnCount, turn.sequence)
    entry.active = true
    entry.unread = false
    store.upsert(entry)
  }

  function applyRevert(scope: SessionUpdates, previous: AgentSession, summary: SessionSummary) {
    scope.invalidate()
    if (session.value !== previous) return
    session.value = {
      ...summary,
      turns: previous.turns.filter((turn) => turn.sequence <= summary.turnCount),
      earlierTurn: previous.earlierTurn && previous.earlierTurn.sequence <= summary.turnCount ? previous.earlierTurn : undefined,
      lastOptions: previous.lastOptions,
    }
    history.value.turns = history.value.turns.filter((turn) => turn.sequence <= summary.turnCount)
    store.upsert(summary)
  }

  function updateSummary(item: SessionSummary) {
    if (session.value?.id !== item.id || !current) return
    current.invalidate()
    session.value.title = item.title
    session.value.defaultTitle = item.defaultTitle
    session.value.pinned = item.pinned
  }

  function resume() {
    refreshPaused.value = false
    current?.wake()
  }

  function resumeVisible() {
    resume()
    if (current && !document.hidden) void markRead(current)
  }

  watch(sessionId, (id, _, onCleanup) => {
    current = undefined
    refreshPaused.value = false
    history.value = { turns: [], nextBeforeSequence: 0 }
    loadingOlder.value = false
    session.value = null
    policy.value = null
    if (!id) return
    const scope: SessionUpdates = new SessionUpdates(id, {
      active: () => session.value?.active ?? true,
      sync: (entry, policy) => applySession(scope, entry, policy),
      progress: (update) => {
        const entry = session.value
        if (!entry) return
        if (entry.earlierTurn?.id === update.id) {
          entry.earlierTurn = update
          return
        }
        const index = entry.turns.findIndex((turn) => turn.id === update.id)
        if (index >= 0) entry.turns[index] = update
      },
      missing: navigation.missing,
      unauthorized: navigation.unauthorized,
      failed: (retrying) => {
        refreshPaused.value = !retrying
        navigation.loadError(t(retrying ? 'chat.loadRetry' : 'chat.loadFailed'))
      },
    })
    current = scope
    onCleanup(() => scope.close())
    void scope.refresh()
  }, { immediate: true })

  watch(() => summary.value?.updatedAt, (newTime) => {
    if (current && session.value && newTime && session.value.updatedAt !== newTime && !session.value.active) void current.refresh()
  })

  watch(() => summary.value?.projectId, (projectId) => {
    const entry = session.value
    if (entry && entry.id === sessionId() && projectId !== undefined) entry.projectId = projectId
  })

  return {
    session, summary, policy, chatSession, runningTurn, hasOlder, loadingOlder, refreshPaused,
    loadOlder, resume, resumeVisible, updateSummary, rememberQueued, applyRevert,
    updates: () => current,
  }
}
