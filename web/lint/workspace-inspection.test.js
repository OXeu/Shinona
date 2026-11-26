import assert from 'node:assert/strict'
import test from 'node:test'

import { diffLines } from '../src/features/sessions/workspaceInspection.ts'

test('workspace diffs highlight edits inside hunks and preserve file headers and whitespace', () => {
  const diff = [
    'diff --git a/example b/example',
    '--- a/example',
    '+++ b/example',
    '@@ -1,2 +1,3 @@',
    '   indented context',
    '--- removed content',
    '+++ added content',
    '+',
    'diff --git a/second b/second',
    '--- a/second',
    '+++ b/second',
  ]
  const lines = diffLines(diff.join('\n') + '\n')
  assert.deepEqual(lines.map(({ line }) => line), diff)
  assert.deepEqual(
    lines.map(({ kind }) => kind),
    ['', '', '', 'hunk', '', 'deletion', 'addition', 'addition', '', '', ''],
  )
})
