import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { setImmediate } from 'node:timers/promises'
import { test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { create, toBinary } from '@bufbuild/protobuf'
import { build } from 'esbuild'

const { outputFiles } = await build({
  stdin: {
    contents: `export { SessionUpdates } from './src/features/sessions/sessionUpdates'
      export { TranscriptPageSchema } from './src/features/sessions/wire/session_transfer_pb'`,
    resolveDir: fileURLToPath(new URL('../', import.meta.url)),
  },
  platform: 'node',
  format: 'esm',
  bundle: true,
  write: false,
  footer: { js: '//# sourceURL=session-live.test.bundle.js' },
})
const bundle = 'data:text/javascript;base64,' + Buffer.from(outputFiles[0].contents).toString('base64')
const { SessionUpdates, TranscriptPageSchema } = await import(bundle)

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

    constructor() {
      super()
      sockets.push(this)
    }

    send() {}

    close() {
      this.closed = true
    }
  }
  Object.defineProperty(globalThis, 'WebSocket', { configurable: true, value: TestSocket })
  return { document, sockets }
}

function transcriptResponse(title = '') {
  const transcript = toBinary(TranscriptPageSchema, create(TranscriptPageSchema, {
    complete: true,
    session: { id: 'session', title, active: true },
    sandboxSettings: {},
  }))
  return new globalThis.Response(transcript, {
    headers: { 'Content-Type': 'application/protobuf' },
  })
}

function sessionUpdates(t) {
  const sync = t.mock.fn()
  const unauthorized = t.mock.fn()
  const updates = new SessionUpdates('session', {
    active: () => true,
    sync,
    progress: () => {},
    unauthorized,
    missing: () => assert.fail('the fixture transcript must remain available'),
    failed: () => assert.fail('the fixture must not report a transient error'),
  })
  t.after(() => updates.close())
  return { sync, unauthorized, updates }
}

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
