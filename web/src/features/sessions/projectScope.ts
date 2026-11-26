export type ProjectTarget =
  | { readonly kind: 'default' }
  | { readonly kind: 'project'; readonly id: string }

export type ProjectScope = ProjectTarget | { readonly kind: 'all' }

export const allProjects = { kind: 'all' } as const
export const defaultProject = { kind: 'default' } as const

export function projectScopeKey(scope: ProjectScope): string {
  return scope.kind === 'project' ? 'project:' + scope.id : scope.kind
}

export function projectTargetId(project: ProjectTarget): string | null {
  return project.kind === 'project' ? project.id : null
}

export function projectTargetFromId(id: string | null): ProjectTarget {
  return id ? { kind: 'project', id } : defaultProject
}

export function includesProject(scope: ProjectScope, id: string | null): boolean {
  return scope.kind === 'all' || projectTargetId(scope) === id
}

type ProjectSelectionAction =
  | { readonly kind: 'session'; readonly project: ProjectTarget }
  | { readonly kind: 'project'; readonly scope: ProjectScope }

export interface DraftProjectState {
  readonly activeSessionId?: string
  readonly pendingSessionId?: string
  readonly lastAction: ProjectSelectionAction | null
  readonly lastSessionProject: ProjectTarget | null
}

export const emptyDraftProjectState: DraftProjectState = {
  lastAction: null,
  lastSessionProject: null,
}

export function resolveDraftProject(state: DraftProjectState): ProjectTarget {
  if (state.lastAction?.kind === 'session') return state.lastAction.project
  if (state.lastAction?.kind === 'project' && state.lastAction.scope.kind !== 'all') return state.lastAction.scope
  return state.lastSessionProject ?? defaultProject
}

export function openSessionInState(state: DraftProjectState, sessionId: string, project?: ProjectTarget): DraftProjectState {
  if (state.activeSessionId === sessionId) {
    return project !== undefined ? resolveSessionProjectInState(state, sessionId, project) : state
  }
  if (project !== undefined) {
    return { activeSessionId: sessionId, lastAction: { kind: 'session', project }, lastSessionProject: project }
  }
  return { ...state, activeSessionId: sessionId, pendingSessionId: sessionId }
}

export function resolveSessionProjectInState(state: DraftProjectState, sessionId: string, project: ProjectTarget): DraftProjectState {
  if (state.activeSessionId !== sessionId) return state
  if (state.pendingSessionId === sessionId) {
    return { activeSessionId: sessionId, lastAction: { kind: 'session', project }, lastSessionProject: project }
  }
  return { ...state, lastSessionProject: project }
}

export function leaveSessionInState(state: DraftProjectState): DraftProjectState {
  return { lastAction: state.lastAction, lastSessionProject: state.lastSessionProject }
}

export function selectProjectInState(state: DraftProjectState, scope: ProjectScope): DraftProjectState {
  return {
    activeSessionId: state.activeSessionId,
    lastAction: { kind: 'project', scope },
    lastSessionProject: state.lastSessionProject,
  }
}

function isRemovedProject(target: ProjectScope | null, hasProject: (id: string) => boolean) {
  return target?.kind === 'project' && !hasProject(target.id)
}

function pruneProjectsInState(state: DraftProjectState, hasProject: (id: string) => boolean): DraftProjectState {
  const lastSessionProject = isRemovedProject(state.lastSessionProject, hasProject)
    ? defaultProject
    : state.lastSessionProject
  let lastAction = state.lastAction
  if (lastAction?.kind === 'session' && isRemovedProject(lastAction.project, hasProject)) {
    lastAction = { kind: 'session', project: defaultProject }
  } else if (lastAction?.kind === 'project' && isRemovedProject(lastAction.scope, hasProject)) {
    lastAction = null
  }
  if (lastSessionProject === state.lastSessionProject && lastAction === state.lastAction) return state
  return { ...state, lastAction, lastSessionProject }
}

export function removeProjectInState(state: DraftProjectState, projectId: string): DraftProjectState {
  return pruneProjectsInState(state, (id) => id !== projectId)
}

export function retainProjectsInState(state: DraftProjectState, validIds: ReadonlySet<string>): DraftProjectState {
  return pruneProjectsInState(state, (id) => validIds.has(id))
}
