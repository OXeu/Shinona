import { nextTick, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { projectTargetId } from './projectScope'
import { sessionApi } from './sessionApi'
import type {
  ComposerAction, ComposerPayload, ComposerRunOptions, ImageAttachment, PendingAction, PendingMessage, SessionSummary,
} from './sessionApi'
import { useSessionStore } from './sessionStore'
import type { useSessionTranscript } from './useSessionTranscript'

export type TurnActionError = 'submit' | 'stop' | 'pending' | 'revert' | 'retry'

export function useTurnActions(
  sessionId: () => string | undefined,
  transcript: ReturnType<typeof useSessionTranscript>,
  setChatError: (source: TurnActionError, message: string) => void,
) {
  const route = useRoute()
  const router = useRouter()
  const store = useSessionStore()
  const { session, policy } = transcript
  const prompt = ref('')
  const images = ref<ImageAttachment[]>([])
  const submitting = ref(false)
  const reverting = ref(false)
  const revertOpen = ref(false)
  const revertTarget = ref<{ turnId: string; headId: string }>()
  let draftSession: SessionSummary | undefined
  let draftRequest: AbortController | undefined

  async function submit(action: ComposerAction, payload: ComposerPayload) {
    if (submitting.value || reverting.value) return
    setChatError('submit', '')
    const message = prompt.value.trim()
    if ((!message && !payload.images.length) || Array.from(message).length > 8_000) {
      setChatError('submit', t('chat.invalidPrompt'))
      return
    }
    if (!sessionId() && action === 'send') {
      await submitDraft(message, payload)
      return
    }
    const scope = transcript.updates()
    if (!scope || !session.value) return

    submitting.value = true
    try {
      if (action === 'steer') {
        const running = transcript.runningTurn.value
        if (!running || payload.images.length) {
          setChatError('submit', t('composer.notRunning'))
          return
        }
        await sessionApi.steer(scope.id, running.id, message, scope.signal)
        if (scope.signal.aborted) return
      } else {
        if (!policy.value) {
          setChatError('submit', t('sandbox.none'))
          return
        }
        const submitRun = await sessionApi.submit(scope.id, message, payload, action === 'queue', scope.signal)
        if (scope.signal.aborted || !session.value) return
        transcript.rememberQueued(scope, submitRun)
      }
      prompt.value = ''
      images.value = []
      scope.schedule()
    } catch (error) {
      if (!scope.signal.aborted) setChatError('submit', messageFor(error, 'chat.submitFailed'))
    } finally {
      if (!scope.signal.aborted) submitting.value = false
    }
  }

  async function submitDraft(message: string, payload: ComposerPayload) {
    const project = store.draftProject
    const from = route.fullPath
    draftRequest = new AbortController()
    const { signal } = draftRequest
    submitting.value = true
    try {
      if (!draftSession || draftSession.projectId !== projectTargetId(project)) {
        const createSession = await sessionApi.create(project, signal)
        if (signal.aborted || route.fullPath !== from) return
        draftSession = createSession
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
    const scope = transcript.updates()
    if (!scope || session.value?.active || submitting.value || reverting.value) return
    setChatError('retry', '')
    submitting.value = true
    try {
      const result = await sessionApi.retry(scope.id, id, options, scope.signal)
      if (scope.signal.aborted) return
      transcript.rememberQueued(scope, result)
      scope.schedule()
    } catch (error) {
      if (!scope.signal.aborted) setChatError('retry', messageFor(error, 'chat.retryFailed'))
    } finally {
      if (!scope.signal.aborted) submitting.value = false
    }
  }

  async function stop(id: string) {
    const scope = transcript.updates()
    if (!scope || submitting.value || reverting.value) return
    setChatError('stop', '')
    submitting.value = true
    try {
      await sessionApi.stop(scope.id, id, scope.signal)
      if (scope.signal.aborted) return
      notify.success(t('composer.stopRequested'))
      scope.schedule()
    } catch (error) {
      if (!scope.signal.aborted) setChatError('stop', messageFor(error, 'composer.stopFailed'))
    } finally {
      if (!scope.signal.aborted) submitting.value = false
    }
  }

  async function changePending(message: PendingMessage, action: PendingAction, runId: string | undefined, options: ComposerRunOptions) {
    const scope = transcript.updates()
    if (!scope || submitting.value || reverting.value) return
    if (action === 'move' && message.kind === 'queue' && (!runId || message.attachments.length)) return
    const signal = scope.signal
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
      if (!signal.aborted) scope.invalidate()
    } catch (error) {
      if (!signal.aborted) setChatError('pending', messageFor(error, 'composer.pendingActionFailed'))
    } finally {
      if (!signal.aborted) {
        submitting.value = false
        scope.schedule()
        void scope.refresh()
      }
    }
  }

  function askRevert(turnId: string) {
    const entry = session.value
    const head = entry?.turns.at(-1)
    if (!head || entry?.active || submitting.value || reverting.value) return
    revertTarget.value = { turnId, headId: head.id }
    revertOpen.value = true
  }

  async function revertMessage() {
    const scope = transcript.updates()
    const target = revertTarget.value
    const entry = session.value
    if (!scope || !target || !entry || submitting.value || reverting.value) return
    setChatError('revert', '')
    reverting.value = true
    try {
      const revertTurn = await sessionApi.revert(scope.id, target.turnId, target.headId, scope.signal)
      if (scope.signal.aborted) return
      transcript.applyRevert(scope, entry, revertTurn.session)
      prompt.value = revertTurn.prompt
      images.value = revertTurn.images
      notify.success(t('chat.reverted'))
    } catch (error) {
      if (!scope.signal.aborted) setChatError('revert', messageFor(error, 'chat.revertFailed'))
    } finally {
      if (!scope.signal.aborted) {
        revertOpen.value = false
        reverting.value = false
        revertTarget.value = undefined
        scope.schedule()
        void scope.refresh()
        await nextTick()
        if (!scope.signal.aborted) document.getElementById('agent-prompt')?.focus()
      }
    }
  }

  watch(sessionId, () => {
    draftRequest?.abort()
    draftSession = undefined
    prompt.value = ''
    images.value = []
    submitting.value = false
    reverting.value = false
    revertOpen.value = false
    revertTarget.value = undefined
  })
  onUnmounted(() => draftRequest?.abort())

  return {
    prompt, images, submitting, reverting, revertOpen,
    submit, retryTurn, stop, changePending, askRevert, revertMessage,
  }
}
