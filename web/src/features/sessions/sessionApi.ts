import { fromBinary } from '@bufbuild/protobuf'

import { apiJSON, request } from '../../shared/http'
import { decodeRunOptions, decodeSandboxSettings, decodeSessionSummary, decodeTurn } from './wire/sessionMessages'
import { TranscriptPageSchema } from './wire/session_transfer_pb'

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
  steering: { id?: string; text: string; delivered: boolean }[]
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

export type PendingMessage = { id: string; prompt: string } & (
  | { kind: 'steering'; runId: string }
  | { kind: 'queue'; attachments: string[] }
)

export type PendingAction = 'move' | 'draft' | 'delete'

interface PendingDraft {
  prompt: string
  images: ImageAttachment[]
}

function sessionPath(id: string) {
  return '/sessions/' + encodeURIComponent(id)
}

function turnPath(sessionId: string, turnId: string) {
  return sessionPath(sessionId) + '/turns/' + encodeURIComponent(turnId)
}

function steeringPath(sessionId: string, runId: string, messageId: string) {
  return turnPath(sessionId, runId) + '/steering/' + encodeURIComponent(messageId)
}

interface TranscriptPage {
  session: AgentSession | null
  sandboxSettings: SandboxSettings
}

async function transcriptPage(id: string, signal: AbortSignal, beforeSequence?: number): Promise<TranscriptPage> {
  const query = beforeSequence === undefined ? '' : '?beforeSequence=' + beforeSequence
  const response = await request('/api/v1' + sessionPath(id) + '/transcript' + query, undefined, signal)
  if (response.headers.get('Content-Type')?.split(';', 1)[0] !== 'application/protobuf') {
    throw new Error('Unexpected transcript content type.')
  }
  const page = fromBinary(TranscriptPageSchema, new Uint8Array(await response.arrayBuffer()))
  if (!page.complete || !page.sandboxSettings) throw new Error('Incomplete transcript response.')
  return {
    session: page.session ? {
      ...decodeSessionSummary(page.session), turns: page.turns.map(decodeTurn),
      nextBeforeSequence: page.nextBeforeSequence, lastOptions: decodeRunOptions(page.lastOptions),
    } : null,
    sandboxSettings: decodeSandboxSettings(page.sandboxSettings),
  }
}

export const sessionApi = {
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
  queueSteering: (sessionId: string, runId: string, messageId: string, options: ComposerRunOptions, signal: AbortSignal) =>
    apiJSON<void>(steeringPath(sessionId, runId, messageId) + '/queue', 'POST', signal, { options }),
  steerQueued: (sessionId: string, turnId: string, runId: string, signal: AbortSignal) =>
    apiJSON<void>(turnPath(sessionId, turnId) + '/steer', 'POST', signal, { runId }),
  takePending: (sessionId: string, message: PendingMessage, signal: AbortSignal) =>
    apiJSON<PendingDraft>(message.kind === 'steering' ? steeringPath(sessionId, message.runId, message.id) :
      turnPath(sessionId, message.id) + '/queue', 'DELETE', signal),
  stop: (sessionId: string, runId: string, signal: AbortSignal) =>
    apiJSON<void>(turnPath(sessionId, runId) + '/stop', 'POST', signal),
}

export function attachmentURL(sessionId: string, turnId: string, index: number) {
  return '/api/v1' + turnPath(sessionId, turnId) + '/attachments/' + index
}
