import { graphql } from '../../shared/graphql'
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

const configFields = 'provider template timeoutSeconds onTimeout'

export const projectSandboxApi = {
  load: (projectId: string, signal: AbortSignal) => graphql<{ projectSandbox: ProjectSandbox }>(`
    query ProjectSandbox($projectId: ID!) {
      projectSandbox(projectId: $projectId) {
        project { id name }
        config { ${configFields} }
        effective { ${configFields} }
        providers { ${configFields} label }
        providerLabel sandboxId state
      }
    }
  `, { projectId }, signal),
  save: (projectId: string, input: ProjectSandboxConfig, signal: AbortSignal, skipCleanupFor?: string) => graphql<{ saveProjectSandbox: boolean }>(`
    mutation SaveProjectSandbox($projectId: ID!, $input: ProjectSandboxInput!, $skipCleanupFor: String) {
      saveProjectSandbox(projectId: $projectId, input: $input, skipCleanupFor: $skipCleanupFor)
    }
  `, { projectId, input, skipCleanupFor }, signal),
  create: (projectId: string, signal: AbortSignal) => graphql<{ createProjectSandbox: boolean }>(`
    mutation CreateProjectSandbox($projectId: ID!) { createProjectSandbox(projectId: $projectId) }
  `, { projectId }, signal),
  destroy: (projectId: string, signal: AbortSignal) => graphql<{ destroyProjectSandbox: boolean }>(`
    mutation DestroyProjectSandbox($projectId: ID!) { destroyProjectSandbox(projectId: $projectId) }
  `, { projectId }, signal),
  recreate: (projectId: string, signal: AbortSignal) => graphql<{ recreateProjectSandbox: boolean }>(`
    mutation RecreateProjectSandbox($projectId: ID!) { recreateProjectSandbox(projectId: $projectId) }
  `, { projectId }, signal),
}
