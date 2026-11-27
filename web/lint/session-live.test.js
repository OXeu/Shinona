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
  const document = {
    hidden: false,
    get visibilityState() {
      return this.hidden ? 'hidden' : 'visible'
    },
  }
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

function sessionUpdates(t, overrides = {}) {
  const sync = t.mock.fn()
  const progress = t.mock.fn()
  const unauthorized = t.mock.fn()
  const updates = new SessionUpdates('session', {
    sync,
    progress,
    unauthorized,
    missing: () => assert.fail('the fixture transcript must remain available'),
    failed: () => assert.fail('the fixture must not report a transient error'),
    ...overrides,
  })
  t.after(() => updates.close())
  return { sync, progress, unauthorized, updates }
}

function sessionTranscript(t) {
  setActivePinia(createPinia())
  const scope = effectScope()
  t.after(() => { scope.stop(); setActivePinia(undefined) })
  return scope.run(() => useSessionTranscript(() => 'session', {
    missing: () => assert.fail('the fixture transcript must remain available'),
    unauthorized: () => assert.fail('the fixture must stay signed in'),
    loadError: (message) => assert.equal(message, ''),
  }))
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
  const transcript = sessionTranscript(t)
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

test('a displaced turn stays separate until pagination fills the gap and queued history survives live sync', async (t) => {
  const { sockets } = browser(t)
  const earlier = { id: 'running', sequence: 1, status: 'RUNNING' }
  const stopped = { id: 'stopped', sequence: 2, status: 'STOPPED' }
  const queued = { id: 'queued', sequence: 3, status: 'QUEUED' }
  const later = { id: 'later', sequence: 4, status: 'STOPPED' }
  t.mock.method(globalThis, 'fetch', async (path) => {
    if (path.endsWith('beforeSequence=3')) return transcriptResponse('', { turns: [earlier, stopped] })
    if (path.endsWith('beforeSequence=4')) return transcriptResponse('', { turns: [queued], nextBeforeSequence: 3 })
    return transcriptResponse('', { turns: [later], earlierTurn: earlier, nextBeforeSequence: 4 })
  })
  const transcript = sessionTranscript(t)
  await setImmediate()
  assert.deepEqual(transcript.chatSession.value.turns.map(({ id }) => id), [later.id])
  assert.equal(transcript.chatSession.value.earlierTurn.id, earlier.id)
  await transcript.loadOlder()
  const [socket] = sockets
  const updated = { ...earlier, output: 'Still running' }
  receive(socket, 1, {
    case: 'sync', value: {
      session: { id: 'session', active: true }, sandboxSettings: {},
      turnIds: [later.id], earlierTurnId: earlier.id, changedTurns: [updated], nextBeforeSequence: 4,
    },
  })
  assert.equal(transcript.hasOlder.value, true)
  assert.deepEqual(transcript.chatSession.value.turns.map(({ id }) => id), [queued.id, later.id])
  assert.equal(transcript.chatSession.value.earlierTurn.output, updated.output)
  await transcript.loadOlder()
  const { earlierTurn, turns } = transcript.chatSession.value
  assert.equal(transcript.hasOlder.value, false)
  assert.equal(earlierTurn, undefined)
  assert.deepEqual(turns.map(({ id }) => id), [earlier.id, stopped.id, queued.id, later.id])
  assert.equal(turns[0].output, updated.output)
})

test('focusing an active unread session skips read markers until the session becomes idle', async (t) => {
  const { document, sockets } = browser(t)
  const turn = { id: 'turn', sequence: 1, status: 'RUNNING' }
  const fetch = t.mock.method(globalThis, 'fetch', async (path) => path.endsWith('/read-marker')
    ? new globalThis.Response(null, { status: 204 })
    : transcriptResponse('', { turns: [turn], session: { id: 'session', active: true, unread: true, turnCount: 1 } }))
  const transcript = sessionTranscript(t)
  await setImmediate()
  transcript.resumeVisible()
  transcript.resumeVisible()
  assert.equal(fetch.mock.callCount(), 1)
  document.hidden = true
  transcript.resumeVisible()
  receive(sockets[0], 1, {
    case: 'sync', value: {
      session: { id: 'session', unread: true, turnCount: 1 }, sandboxSettings: {},
      turnIds: [turn.id], changedTurns: [{ ...turn, status: 'STOPPED' }],
    },
  })
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 1)
  document.hidden = false
  transcript.resumeVisible()
  transcript.resumeVisible()
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 2)
  assert.equal(fetch.mock.calls[1].arguments[1].method, 'PUT')
  assert.equal(transcript.session.value.unread, false)
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

test('quick tab switches keep the live stream and longer hides refresh on return', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] })
  const { document, sockets } = browser(t)
  const fetch = t.mock.method(globalThis, 'fetch', async () => transcriptResponse('Latest conversation'))
  const { sync, updates } = sessionUpdates(t)
  await updates.refresh()
  const [previous] = sockets
  assert.ok(previous)
  receive(previous, 1, { case: 'sync', value: { session: { id: 'session' }, sandboxSettings: {} } })

  document.hidden = true
  updates.wake()
  t.mock.timers.tick(4_999)
  assert.equal(previous.closed, false)
  document.hidden = false
  updates.wake()
  await setImmediate()
  t.mock.timers.tick(1)
  assert.equal(previous.closed, false)
  assert.equal(fetch.mock.callCount(), 1)

  document.hidden = true
  updates.wake()
  t.mock.timers.tick(5_000)
  assert.equal(previous.closed, true)
  assert.equal(updates.signal.aborted, false)
  assert.equal(fetch.mock.callCount(), 1)

  document.hidden = false
  updates.wake()
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 2)
  assert.equal(sync.mock.callCount(), 3)
  assert.equal(sockets.length, 2)
  assert.equal(sockets[1].closed, false)
  updates.close()
  assert.equal(sockets[1].closed, true)
})

test('a disconnected live stream fetches a fresh transcript once at the retry deadline', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] })
  const { sockets } = browser(t)
  const fetch = t.mock.method(globalThis, 'fetch', async () => transcriptResponse())
  const { updates } = sessionUpdates(t)
  await updates.refresh()
  sockets[0].dispatchEvent(Object.assign(new globalThis.Event('close'), { code: 1006 }))
  await setImmediate()
  for (let second = 1; second < 5; second++) {
    t.mock.timers.tick(1_000)
    updates.wake()
    await setImmediate()
    assert.equal(fetch.mock.callCount(), 1)
  }
  t.mock.timers.tick(1_000)
  await setImmediate()
  assert.equal(fetch.mock.callCount(), 2)
  assert.equal(sockets.length, 2)
})

for (const [code, reason] of [[1008, 'policy'], [1009, 'too-large']]) {
  test(`live close code ${code} pauses updates until an explicit retry`, async (t) => {
    t.mock.timers.enable({ apis: ['setTimeout', 'Date'] })
    const { sockets } = browser(t)
    const fetch = t.mock.method(globalThis, 'fetch', async () => transcriptResponse())
    const failed = t.mock.fn()
    const { updates } = sessionUpdates(t, { failed })
    await updates.refresh()
    sockets[0].dispatchEvent(new globalThis.Event('error'))
    sockets[0].dispatchEvent(Object.assign(new globalThis.Event('close'), { code }))
    assert.deepEqual(failed.mock.calls[0].arguments, [false, reason])
    t.mock.timers.tick(30_000)
    updates.wake()
    await updates.refresh()
    assert.equal(fetch.mock.callCount(), 1)
    updates.retryNow()
    await setImmediate()
    assert.equal(fetch.mock.callCount(), 2)
    assert.equal(sockets.length, 2)
  })
}

test('a missing live session closes its socket before navigating away', async (t) => {
  const { sockets } = browser(t)
  t.mock.method(globalThis, 'fetch', async () => transcriptResponse())
  const missing = t.mock.fn(async () => assert.equal(sockets[0].closed, true))
  const { updates } = sessionUpdates(t, { missing })
  await updates.refresh()
  receive(sockets[0], 1, { case: 'sync', value: {} })
  assert.equal(missing.mock.callCount(), 1)
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
