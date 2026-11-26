import { apiJSON } from '../../shared/http'
import type { Project } from './sessionApi'

export interface ProjectSandboxConfig {
  provider: string
  template: string
  timeoutSeconds: number
  onTimeout: string
}

export interface ProjectSandbox {
  project: Project
  config: ProjectSandboxConfig
  effective: ProjectSandboxConfig
  providerLabel: string
  providers: (ProjectSandboxConfig & { label: string })[]
  sandboxId: string
  state: 'NONE' | 'RUNNING' | 'PAUSED' | 'STOPPED' | 'MISSING' | 'UNAVAILABLE'
}


function sandboxPath(projectId: string) {
  return '/projects/' + encodeURIComponent(projectId || 'default') + '/sandbox'
}

export const projectSandboxApi = {
  load: (projectId: string, signal: AbortSignal) =>
    apiJSON<ProjectSandbox>(sandboxPath(projectId), 'GET', signal),
  save: (projectId: string, input: ProjectSandboxConfig, signal: AbortSignal, skipCleanupFor?: string) =>
    apiJSON<void>(sandboxPath(projectId) + '/config', 'PUT', signal, { input, skipCleanupFor }),
  create: (projectId: string, signal: AbortSignal) =>
    apiJSON<void>(sandboxPath(projectId), 'POST', signal),
  destroy: (projectId: string, signal: AbortSignal) =>
    apiJSON<void>(sandboxPath(projectId), 'DELETE', signal),
  recreate: (projectId: string, signal: AbortSignal) =>
    apiJSON<void>(sandboxPath(projectId) + '/recreate', 'POST', signal),
}
