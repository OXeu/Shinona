import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { gzipSync } from 'node:zlib'
import { build } from 'esbuild'
import type { Plugin } from 'vite'
import { arrowFrameGzipBudget } from './arrow-frame-budget.ts'

const id = 'virtual:arrow-frame'
const entry = fileURLToPath(new URL('./arrow-frame-runtime.ts', import.meta.url))
const quickJS = fileURLToPath(new URL('./arrow-frame-quickjs.ts', import.meta.url))
const documentPath = fileURLToPath(new URL('./arrow-frame.html', import.meta.url))
const contractPath = fileURLToPath(new URL('../internal/adapter/pigo/arrow-view.json', import.meta.url))
const require = createRequire(import.meta.url)
const wasmPath = require.resolve('@jitl/quickjs-wasmfile-release-asyncify/wasm')
const sandbox = dirname(require.resolve('@arrow-js/sandbox'))
const vm = join(sandbox, 'host/quickjs.ts')
const vendorModules = new Map([
  ['virtual:arrow-compiler', join(sandbox, 'compiler/module.ts')],
  ['virtual:arrow-renderer', join(sandbox, 'host/renderer.ts')],
  ['virtual:arrow-vm', vm],
])
const watchFiles = [entry, quickJS, documentPath, contractPath]
const devPrefix = '/@shinona/arrow-frame/'

export function arrowFrame(): Plugin {
  let serve = false
  let assets: Promise<{ runtime: Uint8Array; document: Buffer; wasm: Buffer }> | undefined
  function getAssets() {
    if (assets) return assets
    const pending = Promise.all([
      bundleRuntime(),
      readFile(documentPath),
      readFile(wasmPath),
    ]).then(([runtime, document, wasm]) => {
      const compressedWasm = gzipSync(wasm)
      const size = gzipSync(runtime).length + gzipSync(document).length + compressedWasm.length
      if (size > arrowFrameGzipBudget) throw new Error(`ArrowJS frame requires ${size} compressed bytes. Budget is ${arrowFrameGzipBudget}.`)
      return { runtime, document, wasm: compressedWasm }
    }).catch((error: unknown) => {
      if (assets === pending) assets = undefined
      throw error
    })
    assets = pending
    return pending
  }
  return {
    name: 'shinona-arrow-frame',
    configResolved(config) { serve = config.command === 'serve' },
    configureServer(server) {
      server.watcher.add(watchFiles)
      server.watcher.on('change', path => {
        if (watchFiles.includes(path)) assets = undefined
      })
      server.middlewares.use(devPrefix, (request, response, next) => {
        const file = request.url?.split('?', 1)[0]
        if (file !== '/runtime.js' && file !== '/document.html' && file !== '/quickjs.wasm.gz') {
          next()
          return
        }
        void getAssets().then(({ runtime, document, wasm }) => {
          const [type, body] = file === '/runtime.js' ? ['text/javascript', runtime] :
            file === '/document.html' ? ['text/html', document] : ['application/gzip', wasm]
          response.setHeader('Content-Type', type)
          response.end(body)
        }).catch(next)
      })
    },
    resolveId: source => source === id ? '\0' + id : null,
    async load(source) {
      if (source !== '\0' + id) return
      for (const file of watchFiles) this.addWatchFile(file)
      const contract = JSON.parse(await readFile(contractPath, 'utf8')) as typeof import('../internal/adapter/pigo/arrow-view.json')
      // Keep the host's theme data in its lazy chunk.
      const themeExport = 'export const themeTokens = ' + JSON.stringify(contract.themeTokens) + '\n'
      if (serve) {
        return themeExport + `export default ${JSON.stringify({
          runtime: devPrefix + 'runtime.js', document: devPrefix + 'document.html', wasm: devPrefix + 'quickjs.wasm.gz',
        })}`
      }
      const assets = await getAssets()
      const files = [
        ['runtime', 'arrow-frame-runtime.js', assets.runtime],
        ['document', 'arrow-frame.html', assets.document],
        ['wasm', 'arrow-frame-quickjs.wasm.gz', assets.wasm],
      ] as const
      const exports = files.map(([key, name, source]) =>
        `${key}: import.meta.ROLLUP_FILE_URL_${this.emitFile({ type: 'asset', name, source })}`)
      return themeExport + `export default { ${exports.join(', ')} }`
    },
  }
}

async function bundleRuntime() {
  let isolatedVM = false
  const bundle = await build({
    entryPoints: [entry],
    format: 'iife',
    bundle: true,
    minify: true,
    write: false,
    target: 'es2023',
    plugins: [{
      name: 'isolated-arrow-runtime',
      setup(plugin) {
        // The pinned sandbox package only exposes a complete DOM host. Use its
        // JavaScript compiler, renderer and VM directly for this opaque iframe.
        plugin.onResolve({ filter: /^virtual:arrow-/ }, ({ path }) => {
          const file = vendorModules.get(path)
          return file ? { path: file } : undefined
        })
        plugin.onResolve({ filter: /^quickjs-emscripten$/ }, ({ importer }) => {
          if (importer !== vm) return
          isolatedVM = true
          return { path: quickJS }
        })
      },
    }],
  })
  if (!isolatedVM || !bundle.outputFiles[0]) throw new Error('The ArrowJS bundle requires the bounded QuickJS runtime.')
  return bundle.outputFiles[0].contents
}
