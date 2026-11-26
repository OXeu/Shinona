import { apiJSON } from '../../shared/http'
import { projectTargetId } from './projectScope'
import type { ProjectTarget } from './projectScope'
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

function sandboxPath(project: ProjectTarget) {
  return '/projects/' + encodeURIComponent(projectTargetId(project) ?? 'default') + '/sandbox'
}

export const projectSandboxApi = {
  load: (project: ProjectTarget, signal: AbortSignal) =>
    apiJSON<ProjectSandbox>(sandboxPath(project), 'GET', signal),
  save: (project: ProjectTarget, input: ProjectSandboxConfig, signal: AbortSignal, skipCleanupFor?: string) =>
    apiJSON<void>(sandboxPath(project) + '/config', 'PUT', signal, { input, skipCleanupFor }),
  create: (project: ProjectTarget, signal: AbortSignal) =>
    apiJSON<void>(sandboxPath(project), 'POST', signal),
  destroy: (project: ProjectTarget, signal: AbortSignal) =>
    apiJSON<void>(sandboxPath(project), 'DELETE', signal),
  recreate: (project: ProjectTarget, signal: AbortSignal) =>
    apiJSON<void>(sandboxPath(project) + '/recreate', 'POST', signal),
}
