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

test('live session sign-in closure stops refreshing even while hidden', async (t) => {
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
    constructor() {
      super()
      sockets.push(this)
    }

    send() {}

    close() {}
  }
  Object.defineProperty(globalThis, 'WebSocket', { configurable: true, value: TestSocket })
  const transcript = toBinary(TranscriptPageSchema, create(TranscriptPageSchema, {
    complete: true,
    session: { id: 'session', active: true },
    sandboxSettings: {},
  }))
  const fetch = t.mock.method(globalThis, 'fetch', async () => new globalThis.Response(transcript, {
    headers: { 'Content-Type': 'application/protobuf' },
  }))
  const unauthorized = t.mock.fn()
  const updates = new SessionUpdates('session', {
    active: () => true,
    sync: () => {},
    progress: () => {},
    unauthorized,
    missing: () => assert.fail('a revoked login must not report a missing transcript'),
    failed: () => assert.fail('a revoked login must not report a transient error'),
  })
  t.after(() => updates.close())
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
