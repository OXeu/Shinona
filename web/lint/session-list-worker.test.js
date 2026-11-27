import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { EventEmitter, once } from 'node:events'
import { setImmediate } from 'node:timers/promises'
import { test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { MessageChannel } from 'node:worker_threads'
import { build } from 'esbuild'

const { outputFiles } = await build({
  entryPoints: [fileURLToPath(new URL('../src/features/sessions/sessionListWorker.ts', import.meta.url))],
  platform: 'node',
  format: 'esm',
  bundle: true,
  write: false,
})
const bundle = 'data:text/javascript;base64,' + Buffer.from(outputFiles[0].contents).toString('base64')

async function fixture(t) {
  const worker = new globalThis.EventTarget()
  Object.defineProperty(globalThis, 'addEventListener', { configurable: true, value: worker.addEventListener.bind(worker) })
  t.after(() => Reflect.deleteProperty(globalThis, 'addEventListener'))
  const requests = []
  const events = new EventEmitter()
  t.mock.method(globalThis, 'fetch', (_, { signal }) => {
    const response = Promise.withResolvers()
    const canceled = Promise.withResolvers()
    signal.addEventListener('abort', () => canceled.resolve(), { once: true })
    requests.push({
      signal, canceled: canceled.promise,
      reply: (etag) => response.resolve(new globalThis.Response(JSON.stringify({ projects: [], sessions: [] }), {
        headers: { 'Content-Type': 'application/json', ETag: etag },
      })),
    })
    events.emit('request')
    return response.promise
  })
  t.after(() => requests.forEach((request) => request.reply('cleanup')))
  await import(bundle + '#' + encodeURIComponent(t.name))

  function connect() {
    const { port1, port2 } = new MessageChannel()
    t.after(() => {
      port1.close()
      port2.close()
    })
    worker.dispatchEvent(new globalThis.MessageEvent('connect', { ports: [port2] }))
    async function send(message) {
      const received = once(port2, 'message')
      port1.postMessage(message)
      await received
    }
    return {
      port: port1,
      refresh: (id) => send({ type: 'refresh', id, scope: { owner: 'owner', project: { kind: 'all' } } }),
      cancel: (id) => send({ type: 'cancel', id }),
    }
  }

  async function request(index) {
    while (requests.length <= index) await once(events, 'request')
    return requests[index]
  }

  return { connect, requests, request }
}

test('canceling the last refresh aborts its fetch and lets the next refresh start', { timeout: 2_000 }, async (t) => {
  const { connect, requests, request } = await fixture(t)
  const tab = connect()
  await tab.refresh(1)
  const abandoned = await request(0)
  await tab.cancel(1)
  assert.equal(abandoned.signal.aborted, true)

  const nextReply = once(tab.port, 'message')
  await tab.refresh(2)
  const replacement = await request(1)
  replacement.reply('current')
  const [reply] = await nextReply
  assert.equal(reply.id, 2)
  assert.equal(reply.snapshot.etag, 'current')

  // Simulate a response that finishes despite cancellation. It must not
  // replace the cached snapshot or produce a reply for the canceled refresh.
  abandoned.reply('stale')
  await setImmediate()
  const cachedReply = once(tab.port, 'message')
  await tab.refresh(3)
  const [cached] = await cachedReply
  assert.equal(cached.id, 3)
  assert.equal(cached.snapshot.etag, 'current')
  assert.equal(requests.length, 2)
})

test('canceling one tab leaves the shared fetch available to another tab', { timeout: 2_000 }, async (t) => {
  const { connect, requests, request } = await fixture(t)
  const first = connect()
  const second = connect()
  await first.refresh(1)
  const shared = await request(0)
  const secondReply = once(second.port, 'message')
  await second.refresh(1)
  await first.cancel(1)
  assert.equal(shared.signal.aborted, false)
  shared.reply('shared')
  const [reply] = await secondReply
  assert.equal(reply.snapshot.etag, 'shared')
  assert.equal(requests.length, 1)
})

test('closing a tab port releases its pending refresh', { timeout: 2_000 }, async (t) => {
  const { connect, request } = await fixture(t)
  const tab = connect()
  await tab.refresh(1)
  const pending = await request(0)
  tab.port.close()
  await pending.canceled
  assert.equal(pending.signal.aborted, true)
})
