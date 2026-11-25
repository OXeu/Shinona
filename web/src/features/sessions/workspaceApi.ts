import { graphql } from '../../shared/graphql'

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

export const workspaceApi = {
  changes: (sessionId: string, signal: AbortSignal) =>
    graphql<{ workspaceChanges: WorkspaceChanges }>(`
      query Changes($sessionId: ID!) {
        workspaceChanges(sessionId: $sessionId) { branch truncated entries { path status untracked } }
      }
    `, { sessionId }, signal),
  history: (sessionId: string, signal: AbortSignal) =>
    graphql<{ workspaceHistory: WorkspaceList<GitCommit> }>(`
      query GitHistory($sessionId: ID!) {
        workspaceHistory(sessionId: $sessionId) { truncated entries { hash short author date subject } }
      }
    `, { sessionId }, signal),
  directory: (sessionId: string, path: string, signal: AbortSignal) =>
    graphql<{ workspaceDirectory: WorkspaceList<WorkspaceEntry> }>(`
      query Files($sessionId: ID!, $path: String!) {
        workspaceDirectory(sessionId: $sessionId, path: $path) { truncated entries { path name directory } }
      }
    `, { sessionId, path }, signal),
  preview: (sessionId: string, path: string, diff: boolean, revision: string | undefined, signal: AbortSignal) =>
    graphql<{ workspacePreview: WorkspacePreview }>(`
      query Preview($sessionId: ID!, $path: String!, $diff: Boolean!, $revision: String) {
        workspacePreview(sessionId: $sessionId, path: $path, diff: $diff, revision: $revision) { text truncated }
      }
    `, { sessionId, path, diff, revision }, signal),
  commit: (sessionId: string, message: string, paths: string[], signal: AbortSignal) =>
    graphql<{ commitWorkspace: string }>(`
      mutation Commit($sessionId: ID!, $message: String!, $paths: [String!]!) {
        commitWorkspace(sessionId: $sessionId, message: $message, paths: $paths)
      }
    `, { sessionId, message, paths }, signal),
}
