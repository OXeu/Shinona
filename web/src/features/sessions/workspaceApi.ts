import { apiJSON } from '../../shared/http'

export interface ChangedFile {
  path: string
  status: string
  untracked: boolean
}

export interface GitCommit {
  hash: string
  short: string
  author: string
  date: string
  subject: string
}

export interface WorkspaceEntry {
  path: string
  name: string
  directory: boolean
}

export interface WorkspaceList<T> {
  entries: T[]
  truncated: boolean
}

export interface WorkspaceChanges extends WorkspaceList<ChangedFile> {
  branch: string
}

export interface WorkspacePreview {
  text: string
  truncated: boolean
}

function workspacePath(sessionId: string) {
  return '/sessions/' + encodeURIComponent(sessionId) + '/workspace'
}

export const workspaceApi = {
  root: (sessionId: string, signal: AbortSignal) =>
    apiJSON<string>(workspacePath(sessionId) + '/root', 'GET', signal),
  setRoot: (sessionId: string, root: string, signal: AbortSignal) =>
    apiJSON<string>(workspacePath(sessionId) + '/root', 'PUT', signal, { root }),
  changes: (sessionId: string, signal: AbortSignal) =>
    apiJSON<WorkspaceChanges>(workspacePath(sessionId) + '/changes', 'GET', signal),
  history: (sessionId: string, signal: AbortSignal) =>
    apiJSON<WorkspaceList<GitCommit>>(workspacePath(sessionId) + '/history', 'GET', signal),
  directory: (sessionId: string, path: string, signal: AbortSignal) =>
    apiJSON<WorkspaceList<WorkspaceEntry>>(workspacePath(sessionId) + '/files?' + new URLSearchParams({ path }), 'GET', signal),
  preview: (sessionId: string, path: string, diff: boolean, revision: string | undefined, signal: AbortSignal) => {
    const query = new URLSearchParams({ path, diff: String(diff) })
    if (revision !== undefined) query.set('revision', revision)
    return apiJSON<WorkspacePreview>(workspacePath(sessionId) + '/preview?' + query, 'GET', signal)
  },
  commit: (sessionId: string, message: string, paths: string[], signal: AbortSignal) =>
    apiJSON<string>(workspacePath(sessionId) + '/commits', 'POST', signal, { message, paths }),
}
