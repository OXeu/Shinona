import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { once } from 'node:events'
import { createServer } from 'node:http'
import { test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { build } from 'esbuild'

const { outputFiles } = await build({
  entryPoints: [fileURLToPath(new URL('../src/features/sessions/sessionApi.ts', import.meta.url))],
  platform: 'node',
  format: 'esm',
  bundle: true,
  write: false,
})
const { sessionApi } = await import('data:text/javascript;base64,' + Buffer.from(outputFiles[0].contents).toString('base64'))

const cases = [
  { name: 'latest transcript', start: (signal) => sessionApi.detail('session', signal), sendHeaders: true },
  { name: 'older transcript', start: (signal) => sessionApi.olderTurns('session', 5, signal), sendHeaders: true },
  { name: 'read marker', start: (signal) => sessionApi.markRead('session', 5, 'turn', signal), sendHeaders: false },
]

for (const { name, start, sendHeaders } of cases) {
  test(`${name} times out without closing the conversation`, { timeout: 5_000 }, async (t) => {
    const scope = new globalThis.AbortController()
    const deadline = new globalThis.AbortController()
    const requested = Promise.withResolvers()
    const receivedHeaders = Promise.withResolvers()
    const server = createServer((_, response) => {
      if (sendHeaders) {
        response.writeHead(200, { 'Content-Type': 'application/protobuf' })
        response.write(Buffer.from([0x0a]))
      }
      requested.resolve()
    })
    t.after(() => {
      scope.abort()
      server.closeAllConnections()
      return new Promise((resolve) => server.close(resolve))
    })
    server.listen(0, '127.0.0.1')
    await once(server, 'listening')

    const fetch = globalThis.fetch
    const origin = `http://127.0.0.1:${server.address().port}`
    t.mock.method(globalThis, 'fetch', async (path, init) => {
      const response = await fetch(new URL(path, origin), init)
      receivedHeaders.resolve()
      return response
    })
    const timeout = t.mock.method(globalThis.AbortSignal, 'timeout', () => deadline.signal)

    const request = start(scope.signal)
    // Ensure a failed assertion still allows teardown to cancel this request.
    void request.catch(() => {})
    await requested.promise
    if (sendHeaders) await receivedHeaders.promise
    assert.equal(timeout.mock.callCount(), 1)
    assert.equal(timeout.mock.calls[0].arguments[0], 15_000)

    deadline.abort()
    await assert.rejects(request, { name: 'AbortError' })
    assert.equal(scope.signal.aborted, false)
  })
}
