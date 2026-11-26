import { apiJSON } from '../../shared/http'

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
  text?: string
  tool?: string
  input?: string
  output?: string
  status?: RunStatus | ''
  estimatedTokens?: number
}

export interface AgentSession extends SessionSummary {
  turns: Turn[]
  lastOptions?: ComposerRunOptions | null
  nextBeforeSequence?: number
}

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

function sessionPath(id: string) {
  return '/sessions/' + encodeURIComponent(id)
}

function turnPath(sessionId: string, turnId: string) {
  return sessionPath(sessionId) + '/turns/' + encodeURIComponent(turnId)
}

interface TranscriptPage {
  session: AgentSession | null
  sandboxSettings: SandboxSettings
}

function transcriptPage(id: string, signal: AbortSignal, beforeSequence?: number) {
  const query = beforeSequence === undefined ? '' : '?beforeSequence=' + beforeSequence
  return apiJSON<TranscriptPage>(sessionPath(id) + '/transcript' + query, 'GET', signal)
}

export const sessionApi = {
  recent: (projectId: string | null, signal: AbortSignal) => {
    const query = projectId === null ? '' : '?' + new URLSearchParams({ projectId })
    return apiJSON<{ projects: Project[]; sessions: SessionSummary[] }>('/sessions' + query, 'GET', signal)
  },
  detail: (id: string, signal: AbortSignal) => transcriptPage(id, signal),
  olderTurns: (id: string, beforeSequence: number, signal: AbortSignal) =>
    transcriptPage(id, signal, beforeSequence),
  create: (projectId: string | null, signal?: AbortSignal) =>
    apiJSON<SessionSummary>('/sessions', 'POST', signal, { projectId }),
  createProject: (name: string, signal: AbortSignal) =>
    apiJSON<Project>('/projects', 'POST', signal, { name }),
  renameProject: (id: string, name: string, signal: AbortSignal) =>
    apiJSON<Project>('/projects/' + encodeURIComponent(id), 'PATCH', signal, { name }),
  removeProject: (id: string, signal: AbortSignal) =>
    apiJSON<void>('/projects/' + encodeURIComponent(id), 'DELETE', signal),
  update: (id: string, changes: { title?: string; pinned?: boolean }) =>
    apiJSON<SessionSummary>(sessionPath(id), 'PATCH', undefined, changes),
  archive: (id: string) =>
    apiJSON<void>(sessionPath(id), 'DELETE'),
  markRead: (id: string, through: number, throughId: string, signal: AbortSignal) =>
    apiJSON<void>(sessionPath(id) + '/read-marker', 'PUT', signal, { through, throughId }),
  revert: (sessionId: string, turnId: string, expectedHeadId: string, signal: AbortSignal) =>
    apiJSON<{ session: SessionSummary; prompt: string; images: ImageAttachment[] }>(
      turnPath(sessionId, turnId) + '/revert', 'POST', signal, { expectedHeadId }),
  composerOptions: (signal: AbortSignal) =>
    apiJSON<ComposerOptions>('/composer/options', 'GET', signal),
  saveComposerDefaults: (input: ComposerDefaults, signal: AbortSignal) =>
    apiJSON<ComposerDefaults>('/composer/defaults', 'PUT', signal, input),
  submit: (sessionId: string, prompt: string, payload: ComposerPayload, queue: boolean, signal: AbortSignal) =>
    apiJSON<Turn>(sessionPath(sessionId) + '/turns', 'POST', signal, { prompt, payload, queue }),
  retry: (sessionId: string, turnId: string, options: ComposerRunOptions, signal: AbortSignal) =>
    apiJSON<Turn>(turnPath(sessionId, turnId) + '/retry', 'POST', signal, { options }),
  steer: (sessionId: string, runId: string, prompt: string, signal: AbortSignal) =>
    apiJSON<void>(turnPath(sessionId, runId) + '/steering', 'POST', signal, { prompt }),
  stop: (sessionId: string, runId: string, signal: AbortSignal) =>
    apiJSON<void>(turnPath(sessionId, runId) + '/stop', 'POST', signal),
}

export function attachmentURL(sessionId: string, turnId: string, index: number) {
  return '/api/v1' + turnPath(sessionId, turnId) + '/attachments/' + index
}
