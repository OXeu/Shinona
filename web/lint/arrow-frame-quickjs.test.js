import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { test } from 'node:test'

import { initializeArrowWasm, newQuickJSAsyncWASMModule } from '../arrow-frame-quickjs.ts'

const require = createRequire(import.meta.url)

test('ArrowJS bounds guest evaluation, callbacks and batches of pending jobs', async t => {
  const bytes = await readFile(require.resolve('@jitl/quickjs-wasmfile-release-asyncify/wasm'))
  initializeArrowWasm(bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength))
  const engine = await newQuickJSAsyncWASMModule()
  const runtime = engine.newRuntime()
  const context = runtime.newContext()
  let callback
  t.after(() => {
    if (callback?.alive) callback.dispose()
    context.dispose()
    runtime.dispose()
  })
  const loop = 'let total = 0; for (let i = 0; i < 1_000_000; i++) total += i;'
  callback = context.unwrapResult(context.evalCode('(function() { ' + loop + ' })'))

  let now = 0
  let step = 400
  t.mock.method(globalThis.performance, 'now', () => now += step)
  const evaluation = await context.evalCodeAsync(loop)
  assert.throws(() => context.unwrapResult(evaluation), /interrupted/i)
  const call = context.callFunction(callback, context.undefined, [])
  assert.throws(() => context.unwrapResult(call), /interrupted/i)
  callback.dispose()

  context.unwrapResult(context.evalCode('for (let i = 0; i < 64; i++) Promise.resolve().then(() => {});')).dispose()
  assert.throws(() => context.unwrapResult(runtime.executePendingJobs()), /execution limit exceeded|interrupted/i)
  assert.ok(runtime.hasPendingJob())

  step = 0
  assert.equal(context.unwrapResult(runtime.executePendingJobs(1)), 1)
  assert.ok(context.unwrapResult(runtime.executePendingJobs()) > 0)
  assert.equal(runtime.hasPendingJob(), false)
})
