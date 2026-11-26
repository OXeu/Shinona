import assert from 'node:assert/strict'
import test from 'node:test'

import {
  allProjects,
  defaultProject,
  emptyDraftProjectState,
  leaveSessionInState,
  openSessionInState,
  removeProjectInState,
  resolveDraftProject,
  resolveSessionProjectInState,
  retainProjectsInState,
  selectProjectInState,
} from '../src/features/sessions/projectScope.ts'

const projectA = { kind: 'project', id: 'project-a' }
const projectB = { kind: 'project', id: 'project-b' }

test('defaults to the default project when no action has occurred', () => {
  assert.deepEqual(resolveDraftProject(emptyDraftProjectState), defaultProject)
})

test('uses the opened session project when switching to a session', () => {
  let state = openSessionInState(emptyDraftProjectState, 's1', projectA)
  assert.deepEqual(resolveDraftProject(state), projectA)

  state = openSessionInState(state, 's2', defaultProject)
  assert.deepEqual(resolveDraftProject(state), defaultProject)
})

test('prefers the latest project switch over an earlier session switch', () => {
  let state = openSessionInState(emptyDraftProjectState, 's1', projectA)
  state = selectProjectInState(state, projectB)
  assert.deepEqual(resolveDraftProject(state), projectB)

  state = openSessionInState(state, 's2', defaultProject)
  state = selectProjectInState(state, projectA)
  assert.deepEqual(resolveDraftProject(state), projectA)
})

test('prefers the latest session switch over an earlier project switch', () => {
  let state = selectProjectInState(emptyDraftProjectState, projectA)
  state = openSessionInState(state, 's1', projectB)
  assert.deepEqual(resolveDraftProject(state), projectB)
})

test('falls back to the last opened session project or default when switching to all projects', () => {
  const fromEmpty = selectProjectInState(emptyDraftProjectState, allProjects)
  assert.deepEqual(resolveDraftProject(fromEmpty), defaultProject)

  let state = openSessionInState(emptyDraftProjectState, 's1', projectA)
  state = selectProjectInState(state, projectB)
  assert.deepEqual(resolveDraftProject(state), projectB)

  state = selectProjectInState(state, allProjects)
  assert.deepEqual(resolveDraftProject(state), projectA)
})

test('deferred session project resolution respects newer project switches', () => {
  let state = openSessionInState(emptyDraftProjectState, 's1')
  state = resolveSessionProjectInState(state, 's1', projectA)
  assert.deepEqual(resolveDraftProject(state), projectA)

  let overridden = openSessionInState(emptyDraftProjectState, 's2')
  overridden = selectProjectInState(overridden, projectB)
  overridden = resolveSessionProjectInState(overridden, 's2', projectA)
  assert.deepEqual(resolveDraftProject(overridden), projectB)

  overridden = selectProjectInState(overridden, allProjects)
  assert.deepEqual(resolveDraftProject(overridden), projectA)
})

test('leaving a session preserves history while returning to the active session keeps a newer project switch', () => {
  let state = openSessionInState(emptyDraftProjectState, 's1', projectA)
  state = selectProjectInState(state, projectB)

  // Returning from another route like /settings keeps the same activeSessionId.
  const returned = openSessionInState(state, 's1', projectA)
  assert.deepEqual(resolveDraftProject(returned), projectB)

  const drafted = leaveSessionInState(state)
  assert.deepEqual(resolveDraftProject(drafted), projectB)

  const reopened = openSessionInState(drafted, 's1', projectA)
  assert.deepEqual(resolveDraftProject(reopened), projectA)
})

test('removing or pruning deleted projects updates removed targets without overriding valid fallback history', () => {
  let state = openSessionInState(emptyDraftProjectState, 's1', projectA)
  state = selectProjectInState(state, projectB)

  state = removeProjectInState(state, 'project-b')
  assert.deepEqual(resolveDraftProject(state), projectA)

  state = retainProjectsInState(state, new Set())
  assert.deepEqual(resolveDraftProject(state), defaultProject)
})
