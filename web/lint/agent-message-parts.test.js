import { deepEqual } from 'node:assert/strict'
import { test } from 'node:test'

import { messageChunks } from '../src/features/sessions/agentMessageParts.ts'

const parts = [
  { kind: 'text', text: 'Earlier output' },
  { kind: 'thinking', text: 'Check the result' },
  { kind: 'tool', tool: 'read' },
  { kind: 'text', text: 'Last body part' },
]
const indexed = parts.map((part, index) => ({ ...part, index }))

test('successful turns with a final output fold all earlier parts into one group', () => {
  const chunks = messageChunks({ status: 'SUCCEEDED', output: 'Final response', parts })
  deepEqual(chunks, [{ kind: 'process', index: 0, parts: indexed }])
})

test('successful turns without a separate output keep the last text part visible', () => {
  const chunks = messageChunks({ status: 'SUCCEEDED', output: '', parts })
  deepEqual(chunks, [
    { kind: 'process', index: 0, parts: indexed.slice(0, 3) },
    { kind: 'text', part: indexed[3] },
  ])
})

test('rich content remains visible after a successful turn', () => {
  const ui = { kind: 'ui', text: 'Total', output: '{}' }
  const chunks = messageChunks({ status: 'SUCCEEDED', output: 'Final response', parts: [...parts, ui] })
  deepEqual(chunks, [
    { kind: 'process', index: 0, parts: indexed },
    { kind: 'ui', part: { ...ui, index: 4 } },
  ])
})

test('running, failed, and stopped turns keep body parts visible between process groups', () => {
  for (const status of ['RUNNING', 'FAILED', 'STOPPED']) {
    const chunks = messageChunks({ status, output: '', parts })
    deepEqual(chunks, [
      { kind: 'text', part: indexed[0] },
      { kind: 'process', index: 1, parts: indexed.slice(1, 3) },
      { kind: 'text', part: indexed[3] },
    ])
  }
})
