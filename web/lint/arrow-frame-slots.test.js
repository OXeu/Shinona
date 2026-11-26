import assert from 'node:assert/strict'
import { test } from 'node:test'

import { ArrowFrameSlots } from '../src/shared/ui/arrowFrameSlots.ts'

const noop = () => {}

test('visible views receive capacity in order and canceled views leave the queue', async () => {
  const slots = new ArrowFrameSlots()
  const signal = new globalThis.AbortController().signal
  const first = await slots.reserve(signal, noop, noop, false)
  const second = await slots.reserve(signal, noop, noop, false)
  const third = await slots.reserve(signal, noop, noop, false)
  const canceled = new globalThis.AbortController()
  const waiting = []
  const skipped = slots.reserve(canceled.signal, () => waiting.push('skipped'), noop, false)
  const fourth = slots.reserve(signal, () => waiting.push('fourth'), noop, false)
  const fifth = slots.reserve(signal, () => waiting.push('fifth'), noop, false)
  assert.deepEqual(waiting, ['skipped', 'fourth', 'fifth'])
  canceled.abort()
  assert.equal(await skipped, undefined)

  first()
  const releaseFourth = await fourth
  first()
  assert.equal(await Promise.race([fifth, Promise.resolve('still waiting')]), 'still waiting')
  second()
  const releaseFifth = await fifth
  third()
  releaseFourth()
  releaseFifth()
})

test('manual activation gets capacity before the waiting list and revokes only the oldest view', async () => {
  const slots = new ArrowFrameSlots()
  const signal = new globalThis.AbortController().signal
  const revoked = []
  const oldest = await slots.reserve(signal, noop, () => revoked.push('oldest'), false)
  const second = await slots.reserve(signal, noop, () => revoked.push('second'), false)
  const third = await slots.reserve(signal, noop, () => revoked.push('third'), false)
  const waiting = slots.reserve(signal, noop, noop, false)

  const manual = await slots.reserve(signal, noop, noop, true)
  assert.deepEqual(revoked, ['oldest'])
  oldest()
  assert.equal(await Promise.race([waiting, Promise.resolve('still waiting')]), 'still waiting')
  manual()
  const next = await waiting
  next()
  second()
  third()
})
