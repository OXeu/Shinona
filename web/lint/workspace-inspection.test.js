import assert from 'node:assert/strict'
import test from 'node:test'

import { workspacePatchFiles } from '../src/features/sessions/workspaceInspection.ts'

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
  '',
].join('\n')

test('workspace diffs parse into one file diff per changed file', () => {
  const files = workspacePatchFiles(diff)
  assert.deepEqual(files.map(({ name }) => name), ['example', 'second'])
  assert.deepEqual(files.map(({ type }) => type), ['change', 'change'])
  assert.deepEqual(files.map(({ hunks }) => hunks.length), [1, 0])
  assert.deepEqual(files[0]?.hunks[0]?.hunkContent, [
    { type: 'context', lines: 1, additionLineIndex: 0, deletionLineIndex: 0 },
    { type: 'change', deletions: 1, additions: 2, deletionLineIndex: 1, additionLineIndex: 1 },
  ])
})

test('plain file previews do not get diff parsing', () => {
  assert.deepEqual(workspacePatchFiles('plain file\n@@ -1 +1 @@\n+literal plus'), [])
})

test('combined merge diffs fall back to the plain text viewer', () => {
  const combined = [
    'diff --cc settings.ts',
    '--- a/settings.ts',
    '+++ b/settings.ts',
    '@@@ -1,3 -1,3 +1,3 @@@',
    '- first parent',
    ' + added against second parent',
    '',
  ].join('\n')
  assert.deepEqual(workspacePatchFiles(combined), [])
})
