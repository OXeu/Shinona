import assert from 'node:assert/strict'
import { Buffer } from 'node:buffer'
import { setImmediate } from 'node:timers/promises'
import { test } from 'node:test'
import { fileURLToPath, URL } from 'node:url'
import { build } from 'esbuild'

const { outputFiles } = await build({
  stdin: {
    contents: "export { mountArrowFrame } from './src/shared/ui/arrowFrames'",
    resolveDir: fileURLToPath(new URL('../', import.meta.url)),
  },
  platform: 'node',
  format: 'esm',
  bundle: true,
  write: false,
  plugins: [{
    name: 'test-arrow-frame-assets',
    setup(plugin) {
      plugin.onResolve({ filter: /^virtual:arrow-frame$/ }, () => ({ path: 'assets', namespace: 'fixture' }))
      plugin.onLoad({ filter: /.*/, namespace: 'fixture' }, () => ({
        contents: 'export default { document: "/frame.html", runtime: "/runtime.js", wasm: "/quickjs.wasm" }',
      }))
    },
  }],
  footer: { js: '//# sourceURL=arrow-frame-assets.test.bundle.js' },
})
const { mountArrowFrame } = await import('data:text/javascript;base64,' + Buffer.from(outputFiles[0].contents).toString('base64'))

test('Arrow frames cancel the asset download only when the last waiting view leaves', async t => {
  const requests = []
  t.mock.method(globalThis, 'fetch', (path, { signal }) => new Promise((resolve, reject) => {
    requests.push({ path, signal, resolve })
    signal.addEventListener('abort', () => reject(signal.reason), { once: true })
  }))
  function start(slot) {
    const controller = new globalThis.AbortController()
    const result = mountArrowFrame({ slot, title: 'Preview', signal: controller.signal }).catch(error => error)
    t.after(() => controller.abort())
    return { controller, result }
  }

  const first = start()
  const second = start()
  assert.equal(requests.length, 2)
  first.controller.abort()
  await setImmediate()
  assert.equal(requests[0].signal.aborted, false)
  second.controller.abort()
  await setImmediate()
  assert.equal(requests[0].signal.aborted, true)
  assert.equal(requests[1].signal.aborted, true)
  assert.equal((await first.result).name, 'AbortError')
  assert.equal((await second.result).name, 'AbortError')

  const document = {
    baseURI: 'https://shinona.example.test/',
    createElement: () => Object.assign(new globalThis.EventTarget(), {
      sandbox: { add() {} },
      remove() {},
    }),
  }
  Object.defineProperty(globalThis, 'document', { configurable: true, value: document })
  t.after(() => Reflect.deleteProperty(globalThis, 'document'))
  const slot = { append: node => node.dispatchEvent(new globalThis.Event('load')) }
  const next = start(slot)
  assert.equal(requests.length, 4)
  assert.equal(requests[2].signal.aborted, false)
  for (const { path, resolve } of requests.slice(2)) {
    const body = path === '/frame.html' ? 'ARROW_FRAME_NONCE ARROW_FRAME_RUNTIME' :
      new Uint8Array([0, 97, 115, 109])
    resolve(new globalThis.Response(body))
  }
  const lease = await next.result
  await lease.ready
  lease.release()
  const cached = await start(slot).result
  await cached.ready
  cached.release()
  assert.equal(requests.length, 4)
})
