import { computed, inject, onScopeDispose, provide, ref, shallowRef, useId, watch } from 'vue'
import type { InjectionKey, Ref } from 'vue'

import breakpoints from '../../shared/ui/styles/breakpoints.module.less'
import { useMediaQuery } from '../../shared/ui/useMediaQuery'

export type RepositoryView = 'changes' | 'files' | 'history'
export type WorkspaceView = RepositoryView | 'terminal'

interface HeaderContext {
  title: Readonly<Ref<string>>
}

function createWorkbench(active: () => boolean, sessionId: () => string | undefined) {
  const compact = useMediaQuery(breakpoints.session)
  const desktopSessions = ref(true)
  const desktopWorkspace = ref(true)
  const compactPane = ref<'sessions' | 'workspace' | null>(null)
  const view = ref<WorkspaceView>('changes')
  const repositoryView = ref<RepositoryView>('changes')
  const sessionHeader = shallowRef<HeaderContext>()
  const sessionsId = useId()
  const workspaceId = useId()

  const sessionsOpen = computed(() => active() && (compact.value ? compactPane.value === 'sessions' : desktopSessions.value))
  const workspaceOpen = computed(() => active() && !!sessionId() &&
    (compact.value ? compactPane.value === 'workspace' : desktopWorkspace.value))
  const terminalOpen = computed(() => workspaceOpen.value && view.value === 'terminal')

  watch([compact, active], () => {
    compactPane.value = null
  })
  watch(sessionId, () => {
    compactPane.value = null
    selectView('changes')
  })

  function toggleSessions() {
    if (compact.value) compactPane.value = sessionsOpen.value ? null : 'sessions'
    else desktopSessions.value = !desktopSessions.value
  }

  function sessionSelected() {
    if (compactPane.value === 'sessions') compactPane.value = null
  }

  function toggleWorkspace() {
    if (compact.value) compactPane.value = workspaceOpen.value ? null : 'workspace'
    else desktopWorkspace.value = !desktopWorkspace.value
  }

  function selectView(next: WorkspaceView) {
    view.value = next
    if (next !== 'terminal') repositoryView.value = next
  }

  function leaveTerminal() {
    if (view.value === 'terminal') selectView(repositoryView.value)
  }

  function setHeader(header: HeaderContext) {
    sessionHeader.value = header
    onScopeDispose(() => {
      if (sessionHeader.value === header) sessionHeader.value = undefined
    })
  }

  return {
    compact, sessionsId, workspaceId, sessionsOpen, workspaceOpen, terminalOpen, view, repositoryView, sessionHeader,
    toggleSessions, sessionSelected, toggleWorkspace, selectView, leaveTerminal, setHeader,
  }
}

const workbenchKey: InjectionKey<ReturnType<typeof createWorkbench>> = Symbol('workbench')

export function provideWorkbench(active: () => boolean, sessionId: () => string | undefined) {
  provide(workbenchKey, createWorkbench(active, sessionId))
}

export function useWorkbench() {
  return inject(workbenchKey)!
}
