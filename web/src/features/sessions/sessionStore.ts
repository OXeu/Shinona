import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { readComposerSelection, writeComposerSelection } from './composerPreferences'
import type { ComposerSelection } from './composerPreferences'
import { sessionApi } from './sessionApi'
import type { Project, SessionSummary } from './sessionApi'
import type { SessionListSnapshot } from './sessionListApi'
import { SessionListPolling } from './sessionListPolling'

export const useSessionStore = defineStore('sessions', () => {
  const recent = ref<SessionSummary[]>([])
  const projects = ref<Project[]>([])
  const composerSelections = ref(new Map<string | null, ComposerSelection>())
  const projectNames = computed(() => new Map(projects.value.map(({ id, name }) => [id, name])))
  // null includes every project, while an empty string selects the default project.
  const selectedProjectId = ref<string | null>(null)
  let account: string | undefined
  let version = 0
  let projectVersion = 0
  let refreshID = 0
  let serverSnapshot: SessionListSnapshot | undefined
  let polling: SessionListPolling | undefined

  function resetPolling() {
    refreshID++
    serverSnapshot = undefined
    polling?.close()
    polling = undefined
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
    selectedProjectId.value = null
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

  function selectProject(id: string | null) {
    if (id === selectedProjectId.value) return
    version++
    resetPolling()
    selectedProjectId.value = id
    recent.value = []
  }

  async function createProject(name: string, signal: AbortSignal) {
    const requestedFor = account
    const project = await sessionApi.createProject(name, signal)
    if (signal.aborted || requestedFor !== account) return false
    projectVersion++
    polling?.changed()
    projects.value = [...projects.value.filter(({ id }) => id !== project.id), project]
    selectProject(project.id)
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
    if (selectedProjectId.value === id) selectProject(null)
    else recent.value = recent.value.map((item) => item.projectId === id ? { ...item, projectId: null } : item)
    return true
  }

  async function refresh(signal: AbortSignal) {
    const owner = account
    if (!owner) return
    const request = ++refreshID
    const requestedAt = version
    const requestedProjectVersion = projectVersion
    const projectId = selectedProjectId.value
    const poller = polling ??= new SessionListPolling(owner, projectId)
    const response = await poller.refresh(serverSnapshot, signal)
    if (signal.aborted || request !== refreshID) return
    serverSnapshot = response
    if (requestedAt === version) recent.value = response.sessions
    if (requestedProjectVersion === projectVersion) {
      projects.value = response.projects
      if (projectId && !response.projects.some(({ id }) => id === projectId)) selectProject(null)
    }
  }

  function upsert(session: SessionSummary) {
    if (selectedProjectId.value !== null && (session.projectId ?? '') !== selectedProjectId.value) return
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
    recent, projects, projectNames, selectedProjectId, composerSelection, chooseComposerOptions, moveDraftComposerOptions,
    useAccount, refresh, selectProject, createProject, renameProject, removeProject, upsert, remove, markRead,
  }
})
