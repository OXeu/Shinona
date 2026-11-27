import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'

import { readComposerSelection, writeComposerSelection } from './composerPreferences'
import type { ComposerSelection } from './composerPreferences'
import {
  allProjects, emptyDraftProjectState, includesProject, leaveSessionInState, openSessionInState, projectScopeKey,
  removeProjectInState, resolveDraftProject, resolveSessionProjectInState, retainProjectsInState, selectProjectInState,
} from './projectScope'
import type { DraftProjectState, ProjectScope, ProjectTarget } from './projectScope'
import { sessionApi } from './sessionApi'
import type { Project, SessionSummary } from './sessionApi'
import type { SessionListSnapshot } from './sessionListApi'
import { SessionListPolling } from './sessionListPolling'
import { rememberTerminalTab } from './terminalPreferences'

export const useSessionStore = defineStore('sessions', () => {
  const recent = ref<SessionSummary[]>([])
  const projects = ref<Project[]>([])
  const composerSelections = ref(new Map<string | null, ComposerSelection>())
  const projectNames = computed(() => new Map(projects.value.map(({ id, name }) => [id, name])))
  const selectedProject = shallowRef<ProjectScope>(allProjects)
  const draftProjectState = shallowRef<DraftProjectState>(emptyDraftProjectState)
  const draftProject = computed(() => resolveDraftProject(draftProjectState.value))
  let account: string | undefined
  let version = 0
  let projectVersion = 0
  let refreshID = 0
  let serverSnapshot: SessionListSnapshot | undefined
  let polling: SessionListPolling | undefined

  function closePolling() {
    polling?.close()
    polling = undefined
  }

  function resetPolling() {
    refreshID++
    serverSnapshot = undefined
    closePolling()
  }

  function useAccount(owner: string | undefined) {
    if (owner === account) return
    account = owner
    version++
    projectVersion++
    resetPolling()
    recent.value = []
    projects.value = []
    composerSelections.value = new Map()
    selectedProject.value = allProjects
    draftProjectState.value = emptyDraftProjectState
  }

  function composerSelection(session: string | null): ComposerSelection {
    return composerSelections.value.get(session) ?? (account && session ? readComposerSelection(account, session) : {})
  }

  function chooseComposerOptions(session: string | null, patch: ComposerSelection) {
    const selection = { ...composerSelection(session), ...patch }
    composerSelections.value.set(session, selection)
    if (account && session) writeComposerSelection(account, session, selection)
  }

  function moveDraftComposerOptions(session: string) {
    const draft = composerSelections.value.get(null)
    if (draft) chooseComposerOptions(session, draft)
    composerSelections.value.delete(null)
  }

  function setSelectedProject(project: ProjectScope) {
    if (projectScopeKey(project) === projectScopeKey(selectedProject.value)) return
    version++
    resetPolling()
    selectedProject.value = project
    recent.value = []
  }

  function selectProject(project: ProjectScope) {
    draftProjectState.value = selectProjectInState(draftProjectState.value, project)
    setSelectedProject(project)
  }

  function selectDraftProject(project: ProjectTarget) {
    draftProjectState.value = selectProjectInState(draftProjectState.value, project)
  }

  function openSession(id: string, project?: ProjectTarget) {
    draftProjectState.value = openSessionInState(draftProjectState.value, id, project)
  }

  function resolveSessionProject(id: string, project: ProjectTarget) {
    draftProjectState.value = resolveSessionProjectInState(draftProjectState.value, id, project)
  }

  function leaveSession() {
    draftProjectState.value = leaveSessionInState(draftProjectState.value)
  }

  async function createProject(name: string, signal: AbortSignal, filterSidebar = true) {
    const requestedFor = account
    const project = await sessionApi.createProject(name, signal)
    if (signal.aborted || requestedFor !== account) return false
    projectVersion++
    polling?.changed()
    projects.value = [...projects.value.filter(({ id }) => id !== project.id), project]
    const target: ProjectTarget = { kind: 'project', id: project.id }
    if (filterSidebar) selectProject(target)
    else selectDraftProject(target)
    return true
  }

  async function renameProject(id: string, name: string, signal: AbortSignal) {
    const requestedFor = account
    const project = await sessionApi.renameProject(id, name, signal)
    if (signal.aborted || requestedFor !== account) return false
    projectVersion++
    polling?.changed()
    projects.value = projects.value.map((entry) => entry.id === id ? project : entry)
    return true
  }

  async function removeProject(id: string, signal: AbortSignal) {
    const requestedFor = account
    await sessionApi.removeProject(id, signal)
    if (signal.aborted || requestedFor !== account) return false
    projectVersion++
    version++
    polling?.changed()
    projects.value = projects.value.filter((entry) => entry.id !== id)
    draftProjectState.value = removeProjectInState(draftProjectState.value, id)
    const selected = selectedProject.value
    if (selected.kind === 'project' && selected.id === id) setSelectedProject(allProjects)
    else recent.value = recent.value.map((item) => item.projectId === id ? { ...item, projectId: null } : item)
    return true
  }

  async function refresh(signal: AbortSignal) {
    const owner = account
    if (!owner) return
    const request = ++refreshID
    const requestedAt = version
    const requestedProjectVersion = projectVersion
    const selected = selectedProject.value
    const poller = polling ??= new SessionListPolling(owner, selected)
    const response = await poller.refresh(serverSnapshot, signal)
    if (signal.aborted || request !== refreshID) return
    serverSnapshot = response
    if (requestedAt === version) recent.value = response.sessions
    if (requestedProjectVersion === projectVersion) {
      const validIds = new Set(response.projects.map(({ id }) => id))
      draftProjectState.value = retainProjectsInState(draftProjectState.value, validIds)
      projects.value = response.projects
      if (selected.kind === 'project' && !validIds.has(selected.id)) setSelectedProject(allProjects)
    }
  }

  function upsert(session: SessionSummary) {
    if (!includesProject(selectedProject.value, session.projectId)) return
    const { id, projectId, title, defaultTitle, turnCount, updatedAt, pinned, active, unread } = session
    const summary: SessionSummary = { id, projectId, title, defaultTitle, turnCount, updatedAt, pinned, active, unread }
    const previous = recent.value.find((entry) => entry.id === id)
    const keys = Object.keys(summary) as (keyof SessionSummary)[]
    if (previous && keys.every((key) => previous[key] === summary[key])) return
    version++
    polling?.changed()
    recent.value = [summary, ...recent.value.filter((entry) => entry.id !== id)]
  }

  function remove(id: string) {
    version++
    polling?.changed()
    recent.value = recent.value.filter((entry) => entry.id !== id)
    composerSelections.value.delete(id)
    if (account) writeComposerSelection(account, id)
    rememberTerminalTab(account, id, null)
  }

  function markRead(id: string, through: number) {
    const session = recent.value.find((entry) => entry.id === id)
    if (session && session.turnCount <= through && !session.active) {
      version++
      polling?.changed()
      recent.value = recent.value.map((entry) => entry.id === id ? { ...entry, unread: false } : entry)
    }
  }

  return {
    recent, projects, projectNames, selectedProject, draftProject,
    composerSelection, chooseComposerOptions, moveDraftComposerOptions,
    useAccount, refresh, closePolling, selectProject, selectDraftProject,
    openSession, resolveSessionProject, leaveSession,
    createProject, renameProject, removeProject, upsert, remove, markRead,
  }
})
