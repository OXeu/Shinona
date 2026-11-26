import { create, fromBinary, toBinary } from '@bufbuild/protobuf'

import type { AgentSession, SandboxSettings, Turn } from '../sessionApi'
import { decodeRunOptions, decodeSandboxSettings, decodeSessionSummary, decodeTurn, decodeTurnPart } from './sessionMessages'
import { LiveMessageSchema, LiveRequestSchema } from './session_transfer_pb'
import type { LiveProgress, LiveSync } from './session_transfer_pb'

const endpoint = '/api/v1/live'
const maxPayload = 64 * 1024 * 1024

interface LiveCallbacks {
  sync: (session: AgentSession, settings: SandboxSettings) => void
  progress: (turn: Turn) => void
  missing: () => void
}

class SessionState {
  private turns: Map<string, Turn>

  constructor(initial: Turn[]) {
    this.turns = new Map(initial.map((turn) => [turn.id, turn]))
  }

  sync(update: LiveSync, callbacks: LiveCallbacks) {
    if (!update.session) {
      callbacks.missing()
      return
    }
    if (!update.sandboxSettings) throw new Error('Live session is missing settings.')
    const changes = new Map(update.changedTurns.map((item) => [item.id, decodeTurn(item)]))
    const turns = update.turnIds.map((id) => {
      const turn = changes.get(id) ?? this.turns.get(id)
      if (!turn) throw new Error('Live session references an unknown turn.')
      return turn
    })
    this.turns = new Map(turns.map((turn) => [turn.id, turn]))
    callbacks.sync({
      ...decodeSessionSummary(update.session), turns,
      lastOptions: decodeRunOptions(update.lastOptions), nextBeforeSequence: update.nextBeforeSequence,
    },
      decodeSandboxSettings(update.sandboxSettings))
  }

  progress(delta: LiveProgress) {
    const previous = this.turns.get(delta.turnId)
    if (previous?.status !== 'RUNNING') throw new Error('Live progress references an inactive turn.')
    const parts = previous.parts.slice(0, delta.partCount)
    for (const { index, item } of delta.parts) {
      if (index >= delta.partCount) throw new Error('Live progress references a part outside the turn.')
      if (item.case === 'replace') {
        if (index > parts.length) throw new Error('Live progress skipped a part.')
        parts[index] = decodeTurnPart(item.value)
      } else if (item.case === 'append') {
        const part = parts[index]
        if (!part) throw new Error('Live progress has no base part.')
        parts[index] = {
          ...part, text: (part.text ?? '') + item.value.text, output: (part.output ?? '') + item.value.output,
          estimatedTokens: item.value.estimatedTokens,
        }
      } else {
        throw new Error('Live progress is missing a change.')
      }
    }
    if (parts.length !== delta.partCount) throw new Error('Live progress is incomplete.')
    const steering = delta.steering?.messages.map(({ text, delivered }) => ({ text, delivered })) ?? previous.steering
    const liveContext = delta.context ? (delta.context.tokens > 0 ? delta.context : undefined) : previous.liveContext
    const next = { ...previous, parts, steering, liveContext }
    this.turns.set(next.id, next)
    return next
  }
}

export async function watchLiveSession(initial: AgentSession, callbacks: LiveCallbacks, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  const payload = toBinary(LiveRequestSchema, create(LiveRequestSchema, {
    sessionId: initial.id,
    paged: true,
    knownTurns: initial.turns.map(({ id, updatedAt, status }) => ({ id, updatedAt, status })),
  }))
  const state = new SessionState(initial.turns)
  const url = new URL(endpoint, location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  const socket = new WebSocket(url)
  socket.binaryType = 'arraybuffer'

  await new Promise<void>((resolve, reject) => {
    const listeners = new AbortController()
    const options = { signal: listeners.signal }
    let sequence = 0
    let timer = setTimeout(fail, 5_000)
    function close() {
      clearTimeout(timer)
      listeners.abort()
      socket.close()
    }
    function fail() {
      close()
      reject(new Error('Live session connection ended.'))
    }
    signal.addEventListener('abort', () => { close(); resolve() }, options)
    socket.addEventListener('error', fail, options)
    socket.addEventListener('close', fail, options)
    socket.addEventListener('open', () => {
      try {
        socket.send(payload)
      } catch {
        fail()
      }
    }, options)
    socket.addEventListener('message', ({ data }) => {
      try {
        if (!(data instanceof ArrayBuffer) || data.byteLength > maxPayload) throw new Error('Invalid live session packet.')
        const message = fromBinary(LiveMessageSchema, new Uint8Array(data))
        if (message.sequence !== sequence + 1 || (!sequence && message.item.case !== 'sync')) {
          throw new Error('Live session updates arrived out of order.')
        }
        sequence = message.sequence
        clearTimeout(timer)
        timer = setTimeout(fail, 35_000)
        switch (message.item.case) {
          case 'sync': state.sync(message.item.value, callbacks); break
          case 'progress': callbacks.progress(state.progress(message.item.value)); break
          case 'heartbeat': break
          default: throw new Error('Invalid live session message.')
        }
      } catch {
        fail()
      }
    }, options)
  })
}
