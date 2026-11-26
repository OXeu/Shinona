import assert from 'node:assert/strict'
import { test } from 'node:test'

import { RetryBackoff } from '../src/shared/retryBackoff.ts'

test('automatic retries increase their delay, stop, and reset on a fresh request', () => {
  const retry = new RetryBackoff()
  assert.deepEqual(Array.from({ length: 6 }, () => retry.next()), [1000, 2000, 4000, 8000, 16000, 30000])
  assert.equal(retry.next(), undefined)
  assert.equal(retry.next(), undefined)
  retry.reset()
  assert.equal(retry.next(), 1000)
})
