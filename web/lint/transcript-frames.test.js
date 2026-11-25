import { deepEqual, rejects } from 'node:assert/strict'
import { ReadableStream } from 'node:stream/web'
import { test } from 'node:test'

import { transcriptFrames } from '../src/features/sessions/wire/transcriptFrames.ts'

function stream(...parts) {
  return new ReadableStream({
    start(controller) {
      for (const part of parts) controller.enqueue(Uint8Array.from(part))
      controller.close()
    },
  })
}

test('transcript framing handles headers and payloads split across transport chunks', async () => {
  const input = stream([0], [0, 0, 3, 1], [], [2, 3, 0, 0, 0, 1, 4])
  const output = []
  for await (const frame of transcriptFrames(input)) output.push([...frame])
  deepEqual(output, [[1, 2, 3], [4]])
})

test('transcript framing rejects a truncated payload', async () => {
  await rejects(async () => {
    for await (const frame of transcriptFrames(stream([0, 0, 0, 3, 1, 2]))) deepEqual(frame, [])
  }, /ended early/u)
})
