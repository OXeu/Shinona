import assert from 'node:assert/strict'
import process from 'node:process'
import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import { activityCategories, messageChunks } from '../src/features/sessions/agentMessageParts.ts'
import { TranscriptPageSchema } from '../src/features/sessions/wire/session_transfer_pb.ts'

const turns = Array.from({ length: 200 }, (_, index) => ({
  id: `fixture-${index + 1}`,
  sequence: index + 1,
  prompt: 'Review the test results.',
  status: 'SUCCEEDED',
  parts: [
    { kind: 'thinking', text: 'Inspect the workspace.' },
    { kind: 'tool', tool: 'read', input: 'src/component.ts', output: 'export const version = 1' },
    { kind: 'tool', tool: 'bash', input: 'go test ./...', output: 'ok  workspace/internal/component\n'.repeat(64) },
    { kind: 'text', text: 'All tests passed.' },
  ],
}))
const fixture = toBinary(TranscriptPageSchema, create(TranscriptPageSchema, { complete: true, turns }))
const page = fromBinary(TranscriptPageSchema, fixture)
assert(page.complete && page.turns.length === turns.length)
const render = () => page.turns.reduce(
  (count, turn) => count + messageChunks(turn).length + activityCategories(turn.parts, false).length, 0,
)

function measure(run, expected) {
  for (let i = 0; i < 32; i++) assert.equal(run(), expected)
  const samples = Array.from({ length: 7 }, () => {
    let checksum = 0
    const batches = 64
    const start = process.hrtime.bigint()
    for (let i = 0; i < batches; i++) checksum += run()
    const nsPerOp = Number(process.hrtime.bigint() - start) / batches
    assert.equal(checksum, expected * batches)
    return nsPerOp
  }).sort((a, b) => a - b)
  return { latency: { value: samples[3] } }
}

process.stdout.write(JSON.stringify({
  'Web/TranscriptDecode200': measure(() => fromBinary(TranscriptPageSchema, fixture).turns.length, turns.length),
  'Web/AgentMessageParts200': measure(render, turns.length * 4),
}, null, 2) + '\n')
