import { create, fromBinary, toBinary } from '@bufbuild/protobuf'

import { HttpError, request } from '../../shared/http'
import type { AgentSession, Project, SandboxSettings, SessionSummary } from './sessionApi'
import { RecentSocket } from './wire/recentSocket'
import { decodeRunOptions, decodeSandboxSettings, decodeSessionSummary, decodeTurn } from './wire/sessionMessages'
import { RecentPageSchema, RecentRequestSchema, TranscriptChunkSchema } from './wire/session_transfer_pb'
import { transcriptFrames } from './wire/transcriptFrames'

const basePath = '/api/session-transfer/v1'
const transcriptType = 'application/vnd.shinona.transcript.v1+protobuf'
const recentSocket = new RecentSocket()

export function attachmentURL(sessionId: string, turnId: string, index: number) {
  return `${basePath}/transcript/${encodeURIComponent(sessionId)}/turns/${encodeURIComponent(turnId)}/attachments/${index}`
}
let retryRecentAt = 0

async function recent(projectId: string | null, signal: AbortSignal): Promise<{ projects: Project[]; sessions: SessionSummary[] } | undefined> {
  if (Date.now() < retryRecentAt) return
  try {
    const payload = toBinary(RecentRequestSchema, create(RecentRequestSchema, { projectId: projectId ?? undefined }))
    const result = await recentSocket.read(basePath + '/recent', payload, signal)
    const { default: decompress } = await import('brotli/decompress.js')
    const page = fromBinary(RecentPageSchema, decompress(result))
    signal.throwIfAborted()
    return {
      projects: page.projects.map(({ id, name }) => ({ id, name })),
      sessions: page.sessions.map(decodeSessionSummary),
    }
  } catch {
    signal.throwIfAborted()
    recentSocket.close()
    retryRecentAt = Date.now() + 30_000
  }
}

async function detail(id: string, signal: AbortSignal, beforeSequence?: number): Promise<{ session: AgentSession | null; sandboxSettings: SandboxSettings } | undefined> {
  let response
  try {
    response = await request(basePath + '/transcript', { id, paged: true, beforeSequence }, signal)
  } catch (error) {
    signal.throwIfAborted()
    if (error instanceof TypeError || (error instanceof HttpError && [404, 405, 501, 502, 503, 504].includes(error.status))) return
    throw error
  }
  if (response.headers.get('Content-Type') !== transcriptType || !response.body) {
    await response.body?.cancel()
    return
  }
  const { decompress } = await import('fzstd')
  let session: AgentSession | null = null
  let sandboxSettings: SandboxSettings | undefined
  let complete = false
  for await (const frame of transcriptFrames(response.body)) {
    const { item } = fromBinary(TranscriptChunkSchema, decompress(frame))
    if (complete) throw new Error('Unexpected frame after the transcript ended.')
    switch (item.case) {
      case 'header': {
        const { session: received, sandboxSettings: settings, lastOptions, nextBeforeSequence } = item.value
        if (sandboxSettings || !settings) throw new Error('Invalid transcript header.')
        if (received) session = { ...decodeSessionSummary(received), turns: [], lastOptions: decodeRunOptions(lastOptions), nextBeforeSequence }
        sandboxSettings = decodeSandboxSettings(settings)
        break
      }
      case 'turns':
        if (!session) throw new Error('Transcript turns require a session.')
        session.turns.push(...item.value.turns.map(decodeTurn))
        break
      case 'done':
        complete = item.value
        break
    }
  }
  if (!complete || !sandboxSettings) throw new Error('The transcript did not complete.')
  return { session, sandboxSettings }
}

export const sessionTransfer = { recent, detail, closeRecent: () => recentSocket.close() }
