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

test('merge previews highlight changes from each parent and reset at the next file', () => {
  const diff = [
    'diff --cc settings.ts',
    '--- a/settings.ts',
    '+++ b/settings.ts',
    '@@@ -1,3 -1,3 +1,3 @@@',
    '- first parent',
    ' -second parent',
    ' +added against second parent',
    '+ added against first parent',
    '  +context beginning with a plus',
    'diff --combined defaults.ts',
    '--- a/defaults.ts',
    '+++ b/defaults.ts',
    '@@@@ -1 -1 -1 +1 @@@@',
    '  +added against third parent',
  ]
  const lines = diffLines(diff.join('\n'))
  assert.deepEqual(lines.map(({ line }) => line), diff)
  assert.deepEqual(
    lines.map(({ kind }) => kind),
    ['', '', '', 'hunk', 'deletion', 'deletion', 'addition', 'addition', '', '', '', '', 'hunk', 'addition'],
  )
})

test('plain file previews do not get diff highlighting', () => {
  assert.deepEqual(diffLines('plain file\n@@ -1 +1 @@\n+literal plus'), [])
})
