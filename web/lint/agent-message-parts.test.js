import { deepEqual } from 'node:assert/strict'
import { test } from 'node:test'

import { activityCategories, messageChunks } from '../src/features/sessions/agentMessageParts.ts'

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

test('completed transcript parts can omit empty fields', () => {
  const emptyText = { kind: 'text' }
  const emptyTool = { kind: 'tool' }
  const chunks = messageChunks({ status: 'SUCCEEDED', output: '', parts: [emptyTool, ...parts, emptyText] })
  deepEqual(chunks.at(-2), { kind: 'text', part: { ...parts[3], index: 4 } })
  deepEqual(activityCategories([emptyTool], false), ['tool'])
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

test('current activity follows the executing tool while the completed process remains visible', () => {
  const history = [
    { kind: 'thinking', text: 'Check files' },
    { kind: 'tool', tool: 'read', status: 'SUCCEEDED' },
    { kind: 'text', text: 'The files look good.' },
    { kind: 'thinking', text: 'Run the tests' },
    { kind: 'tool', tool: 'bash', status: 'RUNNING' },
    { kind: 'tool', tool: 'write', status: 'QUEUED' },
  ]
  const items = history.map((part, index) => ({ ...part, index }))
  const chunks = messageChunks({ status: 'RUNNING', output: '', parts: history })
  deepEqual(chunks, [
    { kind: 'process', index: 0, parts: items.slice(0, 2) },
    { kind: 'text', part: items[2] },
    { kind: 'process', index: 3, parts: items.slice(3) },
  ])
  deepEqual(activityCategories(chunks[2].parts, true), ['command'])
  deepEqual(activityCategories(chunks[0].parts, false), ['read'])
})
