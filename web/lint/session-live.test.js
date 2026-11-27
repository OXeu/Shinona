import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { setImmediate } from 'node:timers/promises'
import { test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { build } from 'esbuild'

const { outputFiles } = await build({
  stdin: {
    contents: `export { SessionUpdates } from './src/features/sessions/sessionUpdates'
      export { useSessionTranscript } from './src/features/sessions/useSessionTranscript'
      export { createPinia, setActivePinia } from 'pinia'
      export { effectScope } from 'vue'
      export { LiveMessageSchema, LiveRequestSchema, TranscriptPageSchema } from './src/features/sessions/wire/session_transfer_pb'`,
    resolveDir: fileURLToPath(new URL('../', import.meta.url)),
  },
  platform: 'node',
  format: 'esm',
  bundle: true,
  write: false,
  plugins: [{
    name: 'test-shared-worker',
    setup(plugin) {
      plugin.onResolve({ filter: /\?sharedworker$/ }, ({ path }) => ({ path, namespace: 'shared-worker' }))
      plugin.onLoad({ filter: /.*/, namespace: 'shared-worker' }, () => ({
        contents: 'export default class { constructor() { throw new Error("Unexpected session list worker") } }',
      }))
    },
  }],
  footer: { js: '//# sourceURL=session-live.test.bundle.js' },
})
const bundle = 'data:text/javascript;base64,' + Buffer.from(outputFiles[0].contents).toString('base64')
const {
  SessionUpdates, useSessionTranscript, createPinia, setActivePinia, effectScope,
  LiveMessageSchema, LiveRequestSchema, TranscriptPageSchema,
} = await import(bundle)

function browser(t) {
  const document = { hidden: false }
  const originalWebSocket = Object.getOwnPropertyDescriptor(globalThis, 'WebSocket')
  Object.defineProperty(globalThis, 'document', { configurable: true, value: document })
  Object.defineProperty(globalThis, 'location', { configurable: true, value: { href: 'https://shinona.example.test/' } })
  t.after(() => {
    Reflect.deleteProperty(globalThis, 'document')
    Reflect.deleteProperty(globalThis, 'location')
    if (originalWebSocket) Object.defineProperty(globalThis, 'WebSocket', originalWebSocket)
    else Reflect.deleteProperty(globalThis, 'WebSocket')
  })
  const sockets = []
  class TestSocket extends globalThis.EventTarget {
    closed = false
    sent = []

    constructor() {
      super()
      sockets.push(this)
    }

    send(data) {
      this.sent.push(data)
    }

    close() {
      this.closed = true
    }
  }
  Object.defineProperty(globalThis, 'WebSocket', { configurable: true, value: TestSocket })
  return { document, sockets }
}

function transcriptResponse(title = '', fields = {}) {
  const transcript = toBinary(TranscriptPageSchema, create(TranscriptPageSchema, {
    complete: true,
    session: { id: 'session', title, active: true },
    sandboxSettings: {},
    ...fields,
  }))
  return new globalThis.Response(transcript, {
    headers: { 'Content-Type': 'application/protobuf' },
  })
}

function sessionUpdates(t) {
  const sync = t.mock.fn()
  const progress = t.mock.fn()
  const unauthorized = t.mock.fn()
  const updates = new SessionUpdates('session', {
    active: () => true,
    sync,
    progress,
    unauthorized,
    missing: () => assert.fail('the fixture transcript must remain available'),
    failed: () => assert.fail('the fixture must not report a transient error'),
  })
  t.after(() => updates.close())
  return { sync, progress, unauthorized, updates }
}

function receive(socket, sequence, item) {
  const data = toBinary(LiveMessageSchema, create(LiveMessageSchema, { sequence, item }))
  socket.dispatchEvent(new globalThis.MessageEvent('message', { data: data.slice().buffer }))
}

test('live updates and completion reconcile a running turn outside the latest page', async (t) => {
  const { sockets } = browser(t)
  const part = { kind: 'text', text: 'Watching' }
  const earlier = { id: 'earlier', sequence: 1, status: 'RUNNING', parts: [part] }
  const later = { id: 'later', sequence: 10, status: 'STOPPED' }
  t.mock.method(globalThis, 'fetch', async () => transcriptResponse('', {
    turns: [later], earlierTurn: earlier, nextBeforeSequence: later.sequence,
  }))
  const { sync, progress, updates } = sessionUpdates(t)
  await updates.refresh()
  assert.equal(sync.mock.calls[0].arguments[0].earlierTurn.id, earlier.id)
  const [socket] = sockets
  socket.dispatchEvent(new globalThis.Event('open'))
  const request = fromBinary(LiveRequestSchema, socket.sent[0])
  assert.equal(request.includeEarlierTurn, true)
  assert.deepEqual(request.knownTurns.map(({ id }) => id), [later.id, earlier.id])

  const snapshot = {
    session: { id: 'session', active: true }, sandboxSettings: {},
    turnIds: [later.id], earlierTurnId: earlier.id, nextBeforeSequence: later.sequence,
  }
  receive(socket, 1, { case: 'sync', value: { ...snapshot, changedTurns: [earlier] } })
  receive(socket, 2, {
    case: 'progress', value: {
      turnId: earlier.id, partCount: 1, parts: [{ index: 0, item: { case: 'append', value: { text: ' closely' } } }],
    },
  })
  assert.equal(progress.mock.calls[0].arguments[0].parts[0].text, 'Watching closely')
  const finished = { ...earlier, status: 'SUCCEEDED', output: 'Done.' }
  receive(socket, 3, {
    case: 'sync', value: { ...snapshot, session: { id: 'session', active: false }, changedTurns: [finished] },
  })
  receive(socket, 4, { case: 'sync', value: { ...snapshot, session: { id: 'session', active: false } } })
  const [page] = sync.mock.calls.at(-1).arguments
  assert.deepEqual(page.turns.map(({ id }) => id), [later.id])
  assert.equal(page.nextBeforeSequence, later.sequence)
  assert.equal(page.earlierTurn.output, finished.output)
  assert.equal(socket.closed, false)
})

test('loading older history keeps a displaced running turn live and displays it once', async (t) => {
  const { sockets } = browser(t)
  const earlier = { id: 'running', sequence: 3, status: 'RUNNING', parts: [{ kind: 'text', text: 'Working' }] }
  const later = { id: 'stopped', sequence: 4, status: 'STOPPED' }
  const previous = { id: 'previous', sequence: 2, status: 'SUCCEEDED' }
  t.mock.method(globalThis, 'fetch', async (path) => path.endsWith('beforeSequence=4')
    ? transcriptResponse('', { turns: [previous, earlier] })
    : transcriptResponse('', { turns: [later], earlierTurn: earlier, nextBeforeSequence: later.sequence }))
  setActivePinia(createPinia())
  const scope = effectScope()
  t.after(() => { scope.stop(); setActivePinia(undefined) })
  const transcript = scope.run(() => useSessionTranscript(() => 'session', {
    missing: () => assert.fail('the fixture transcript must remain available'),
    unauthorized: () => assert.fail('the fixture must stay signed in'),
    loadError: (message) => assert.equal(message, ''),
  }))
  await setImmediate()
  assert.equal(transcript.runningTurn.value.id, earlier.id)
  assert.equal(transcript.hasOlder.value, true)
  await transcript.loadOlder()
  assert.equal(transcript.hasOlder.value, false)
  const [socket] = sockets
  receive(socket, 1, {
    case: 'sync', value: {
      session: { id: 'session', active: true }, sandboxSettings: {},
      turnIds: [later.id], earlierTurnId: earlier.id, changedTurns: [earlier], nextBeforeSequence: later.sequence,
    },
  })
  receive(socket, 2, {
    case: 'progress', value: {
      turnId: earlier.id, partCount: 1, parts: [{ index: 0, item: { case: 'append', value: { text: ' again' } } }],
    },
  })
  const shown = transcript.chatSession.value.turns
  assert.deepEqual(shown.map(({ id }) => id), [previous.id, earlier.id, later.id])
  assert.equal(shown[1].parts[0].text, 'Working again')
  assert.equal(transcript.hasOlder.value, false)
})

test('live session sign-in closure stops refreshing even while hidden', async (t) => {
  const { document, sockets } = browser(t)
  const fetch = t.mock.method(globalThis, 'fetch', async () => transcriptResponse())
  const { unauthorized, updates } = sessionUpdates(t)
  await updates.refresh()
  const [socket] = sockets
  assert.ok(socket)
  document.hidden = true
  socket.dispatchEvent(Object.assign(new globalThis.Event('close'), { code: 4401 }))
  await setImmediate()
  assert.equal(unauthorized.mock.callCount(), 1)
  assert.equal(updates.signal.aborted, true)
  document.hidden = false
  updates.wake()
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 1)
})

test('hiding the tab closes the live stream and showing it reconciles the latest transcript', async (t) => {
  const { document, sockets } = browser(t)
  const fetch = t.mock.method(globalThis, 'fetch', async () => transcriptResponse('Latest conversation'))
  const { sync, updates } = sessionUpdates(t)
  await updates.refresh()
  const [previous] = sockets
  assert.ok(previous)

  document.hidden = true
  updates.wake()
  assert.equal(previous.closed, true)
  assert.equal(updates.signal.aborted, false)
  assert.equal(fetch.mock.callCount(), 1)

  document.hidden = false
  updates.wake()
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 2)
  assert.equal(sync.mock.callCount(), 2)
  assert.equal(sockets.length, 2)
  assert.equal(sockets[1].closed, false)
  updates.close()
  assert.equal(sockets[1].closed, true)
})

test('a response started before hiding the tab cannot replace the resumed transcript', async (t) => {
  const { document, sockets } = browser(t)
  const previousResponse = Promise.withResolvers()
  t.after(() => previousResponse.resolve(transcriptResponse()))
  let pending = true
  const fetch = t.mock.method(globalThis, 'fetch', async () => {
    if (!pending) return transcriptResponse('Current transcript')
    pending = false
    return previousResponse.promise
  })
  const { sync, updates } = sessionUpdates(t)
  const previous = updates.refresh()
  const previousSignal = fetch.mock.calls[0].arguments[1].signal

  document.hidden = true
  updates.wake()
  assert.equal(previousSignal.aborted, true)
  assert.equal(updates.signal.aborted, false)

  document.hidden = false
  updates.wake()
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 2)
  previousResponse.resolve(transcriptResponse('Stale transcript'))
  await previous
  assert.equal(sync.mock.callCount(), 1)
  assert.equal(sync.mock.calls[0].arguments[0].title, 'Current transcript')
  assert.equal(sockets.length, 1)
})
