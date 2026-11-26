import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join, resolve } from 'node:path'
import process from 'node:process'
import { gzipSync } from 'node:zlib'
import { arrowFrameGzipBudget } from '../arrow-frame-budget.ts'

const dist = resolve(process.argv[2] ?? 'dist')
const manifest = JSON.parse(readFileSync(join(dist, '.vite/manifest.json'), 'utf8'))
const js = new Set()
const css = new Set()
const visited = new Set()

function visit(key) {
  if (visited.has(key)) return
  visited.add(key)
  const entry = manifest[key]
  assert(entry, `missing manifest entry ${key}`)
  js.add(entry.file)
  for (const path of entry.css ?? []) css.add(path)
  for (const dependency of entry.imports ?? []) visit(dependency)
}

assert(manifest['index.html']?.isEntry, 'missing Vite application entry')
visit('index.html')
assert(css.size > 0, 'missing application CSS')

const files = readdirSync(dist, { recursive: true, withFileTypes: true })
function emittedAsset(stem, suffix) {
  const matches = files.filter(file => file.isFile() && file.name.startsWith(stem + '-') && file.name.endsWith(suffix))
  assert.equal(matches.length, 1, `expected one emitted ${stem}${suffix}`)
  return join(matches[0].parentPath, matches[0].name)
}

function size(paths) {
  let raw = 0
  let gzip = 0
  for (const path of paths) {
    const data = readFileSync(path)
    assert(data.length > 0, `empty build asset ${path}`)
    raw += data.length
    gzip += path.endsWith('.gz') ? data.length : gzipSync(data).length
  }
  return { 'raw-bytes': { value: raw }, 'gzip-bytes': { value: gzip } }
}

const arrow = size([
  emittedAsset('arrow-frame-runtime', '.js'),
  emittedAsset('arrow-frame', '.html'),
  emittedAsset('arrow-frame-quickjs.wasm', '.gz'),
])
assert(arrow['gzip-bytes'].value <= arrowFrameGzipBudget, 'Arrow frame exceeds its gzip budget')
process.stdout.write(JSON.stringify({
  'Web/MainJS': size([...js].map(path => join(dist, path))),
  'Web/MainCSS': size([...css].map(path => join(dist, path))),
  'Web/ArrowFrame': arrow,
}, null, 2) + '\n')
