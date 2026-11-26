import { graphql } from '../../shared/graphql'
import { sessionTransfer } from './sessionTransfer'

export type RunStatus = 'QUEUED' | 'RUNNING' | 'SUCCEEDED' | 'FAILED' | 'STOPPED'
export type Policy = 'FRESH' | 'SHARED'

export interface SandboxSettings {
  available: Policy[]
  selected: Policy | null
}

export interface SessionSummary {
  id: string
  projectId: string | null
  title: string
  defaultTitle: boolean
  turnCount: number
  updatedAt: string
  pinned: boolean
  active: boolean
  unread: boolean
}

export interface Project {
  id: string
  name: string
}

export interface Turn {
  id: string
  sequence: number
  prompt: string
  status: RunStatus
  policy: Policy | null
  output: string
  parts: TurnPart[]
  createdAt: string
  startedAt: string | null
  updatedAt: string
  model: string
  attachments: string[]
  steering: { text: string; delivered: boolean }[]
  contextTokens: number
  contextWindow: number
  tokenUsage?: TokenUsage | null
  liveContext?: ContextUsage | null
}

export interface TokenUsage {
  input: number
  cached: number
  output: number
}

export interface ContextUsage {
  tokens: number
  window: number
  estimated: boolean
}

export interface TurnPart {
  kind: 'text' | 'thinking' | 'tool' | 'failure' | 'ui'
  text: string
  tool: string
  input: string
  output: string
  status: RunStatus | ''
  estimatedTokens?: number
}

export interface AgentSession extends SessionSummary {
  turns: Turn[]
  lastOptions?: ComposerRunOptions | null
  nextBeforeSequence?: number
}

const sessionFields = 'id projectId title defaultTitle turnCount updatedAt pinned active unread'
const turnFields = `id sequence prompt status policy output createdAt startedAt updatedAt model attachments
  steering { text delivered } parts { kind text tool input output status estimatedTokens }
  contextTokens contextWindow liveContext { tokens window estimated } tokenUsage { input cached output }`

export interface ComposerOptions {
  defaultModel: string
  models: { id: string; contextWindow: number }[]
  modelsLoading: boolean
  mcpServers: string[]
  defaults?: ComposerDefaults | null
}

export interface ComposerDefaults {
  model: string
  permission: string
  thinking: string
}

export interface ImageAttachment {
  name: string
  mediaType: string
  data: string
}

export interface ComposerRunOptions {
  model: string
  permission: string
  thinking: string
  mcpServers: string[]
  arrowJs: boolean
}

export interface ComposerPayload extends ComposerRunOptions {
  images: ImageAttachment[]
}

export type ComposerAction = 'send' | 'queue' | 'steer'

const settingsFields = 'available selected'

async function transcriptPage(id: string, signal: AbortSignal, beforeSequence?: number) {
  return await sessionTransfer.detail(id, signal, beforeSequence) ??
    graphql<{ session: AgentSession | null; sandboxSettings: SandboxSettings }>(`
      query Session($id: ID!, $beforeSequence: Int) {
        session(id: $id, beforeSequence: $beforeSequence) {
          ${sessionFields}
          nextBeforeSequence
          lastOptions { model permission thinking mcpServers arrowJs }
          turns { ${turnFields} }
        }
        sandboxSettings { ${settingsFields} }
      }
    `, { id, beforeSequence }, signal)
}

export const sessionApi = {
  recent: async (projectId: string | null, signal: AbortSignal) =>
    await sessionTransfer.recent(projectId, signal) ?? graphql<{
      projects: Project[]
      sessions: SessionSummary[]
    }>(`
      query Sessions($projectId: ID) {
        projects { id name }
        sessions(projectId: $projectId) { ${sessionFields} }
      }
    `, { projectId }, signal),
  closeRecent: sessionTransfer.closeRecent,
  detail: (id: string, signal: AbortSignal) => transcriptPage(id, signal),
  olderTurns: (id: string, beforeSequence: number, signal: AbortSignal) =>
    transcriptPage(id, signal, beforeSequence),
  create: (projectId: string | null, signal?: AbortSignal) =>
    graphql<{ createSession: SessionSummary }>(`
      mutation CreateSession($projectId: ID) { createSession(projectId: $projectId) { ${sessionFields} } }
    `, { projectId }, signal),
  createProject: (name: string, signal: AbortSignal) =>
    graphql<{ createProject: Project }>(`
      mutation CreateProject($name: String!) { createProject(name: $name) { id name } }
    `, { name }, signal),
  renameProject: (id: string, name: string, signal: AbortSignal) =>
    graphql<{ renameProject: Project }>(`
      mutation RenameProject($id: ID!, $name: String!) { renameProject(id: $id, name: $name) { id name } }
    `, { id, name }, signal),
  removeProject: (id: string, signal: AbortSignal) =>
    graphql<{ removeProject: boolean }>(
      'mutation RemoveProject($id: ID!) { removeProject(id: $id) }', { id }, signal),
  update: (id: string, changes: { title?: string; pinned?: boolean }) =>
    graphql<{ updateSession: SessionSummary }>(`
      mutation UpdateSession($id: ID!, $title: String, $pinned: Boolean) {
        updateSession(id: $id, title: $title, pinned: $pinned) { ${sessionFields} }
      }
    `, { id, ...changes }),
  archive: (id: string) =>
    graphql<{ archiveSession: boolean }>('mutation ArchiveSession($id: ID!) { archiveSession(id: $id) }', { id }),
  markRead: (id: string, through: number, throughId: string, signal: AbortSignal) =>
    graphql<{ markSessionRead: boolean }>(`
      mutation MarkSessionRead($id: ID!, $through: Int!, $throughId: ID!) {
        markSessionRead(id: $id, through: $through, throughId: $throughId)
      }
    `, { id, through, throughId }, signal),
  revert: (sessionId: string, turnId: string, expectedHeadId: string, signal: AbortSignal) =>
    graphql<{ revertTurn: { session: SessionSummary; prompt: string; images: ImageAttachment[] } }>(`
      mutation RevertTurn($sessionId: ID!, $turnId: ID!, $expectedHeadId: ID!) {
        revertTurn(sessionId: $sessionId, turnId: $turnId, expectedHeadId: $expectedHeadId) {
          session { ${sessionFields} }
          prompt
          images { name mediaType data }
        }
      }
    `, { sessionId, turnId, expectedHeadId }, signal),
  composerOptions: (signal: AbortSignal) =>
    graphql<{ composerOptions: ComposerOptions }>(`
      query ComposerOptions {
        composerOptions { defaultModel models { id contextWindow } modelsLoading mcpServers defaults { model permission thinking } }
      }
    `, {}, signal),
  saveComposerDefaults: (input: ComposerDefaults, signal: AbortSignal) =>
    graphql<{ setComposerDefaults: ComposerDefaults }>(`
      mutation SetComposerDefaults($input: ComposerDefaultsInput!) {
        setComposerDefaults(input: $input) { model permission thinking }
      }
    `, { input }, signal),
  submit: (sessionId: string, prompt: string, payload: ComposerPayload, queue: boolean, signal: AbortSignal) =>
    graphql<{ submitRun: Turn }>(`
      mutation SubmitTurn($sessionId: ID!, $prompt: String!, $payload: RunPayloadInput!, $queue: Boolean!) {
        submitRun(sessionId: $sessionId, prompt: $prompt, payload: $payload, queue: $queue) { ${turnFields} }
      }
    `, { sessionId, prompt, payload, queue }, signal),
  retry: (sessionId: string, turnId: string, options: ComposerRunOptions, signal: AbortSignal) =>
    graphql<{ retryTurn: Turn }>(`
      mutation RetryTurn($sessionId: ID!, $turnId: ID!, $options: RetryOptionsInput!) {
        retryTurn(sessionId: $sessionId, turnId: $turnId, options: $options) { ${turnFields} }
      }
    `, { sessionId, turnId, options }, signal),
  steer: (sessionId: string, runId: string, prompt: string, signal: AbortSignal) =>
    graphql<{ steerRun: boolean }>(`
      mutation Steer($sessionId: ID!, $runId: ID!, $prompt: String!) {
        steerRun(sessionId: $sessionId, runId: $runId, prompt: $prompt)
      }
    `, { sessionId, runId, prompt }, signal),
  stop: (sessionId: string, runId: string, signal: AbortSignal) =>
    graphql<{ stopRun: boolean }>(`
      mutation Stop($sessionId: ID!, $runId: ID!) { stopRun(sessionId: $sessionId, runId: $runId) }
    `, { sessionId, runId }, signal),
}
