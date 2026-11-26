import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import process from 'node:process'

import { outlineOffsetDeclarations, outlineOffsetMessage } from './style-policy.js'

const repoRoot = join(import.meta.dirname, '..', '..')
const ignoredNames = new Set(['.git', '.tmp', 'node_modules'])
const generatedDirectories = new Set(['bin', 'web/dist'])
const unsupportedExtension = /\.(?:mjs|cjs|mts|cts|jsx|tsx)$/iu

function* sourceFiles(directory, prefix = '') {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (ignoredNames.has(entry.name)) continue
    const path = prefix + entry.name
    if (entry.isDirectory()) {
      if (!generatedDirectories.has(path)) {
        yield* sourceFiles(join(directory, entry.name), path + '/')
      }
    } else {
      yield { path, regularFile: entry.isFile() }
    }
  }
}

function checkStylesheet(path) {
  const stylesheet = readFileSync(join(repoRoot, path), 'utf8')
  try {
    for (const { line, column } of outlineOffsetDeclarations(stylesheet, path)) {
      process.stderr.write(`${path}:${line}:${column} ${outlineOffsetMessage}\n`)
      process.exitCode = 1
    }
  } catch (error) {
    if (error.name !== 'CssSyntaxError') throw error
    process.stderr.write(`${path}:${error.line}:${error.column} ${error.reason}\n`)
    process.exitCode = 1
  }
}

const unsupportedFiles = []
for (const { path, regularFile } of sourceFiles(repoRoot)) {
  if (unsupportedExtension.test(path)) unsupportedFiles.push(path)
  if (regularFile && /\.(?:css|less)$/iu.test(path)) checkStylesheet(path)
}

if (unsupportedFiles.length > 0) {
  process.stderr.write('Use .js or .ts for JavaScript and TypeScript source files.\n')
  process.stderr.write(unsupportedFiles.sort().join('\n') + '\n')
  process.exitCode = 1
}
