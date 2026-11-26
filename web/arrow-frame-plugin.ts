import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { gzipSync } from 'node:zlib'
import { build } from 'esbuild'
import type { Plugin } from 'vite'
import { arrowFrameGzipBudget } from './arrow-frame-budget.ts'

const id = 'virtual:arrow-frame'
const entry = fileURLToPath(new URL('./arrow-frame-runtime.ts', import.meta.url))
const wasmState = fileURLToPath(new URL('./arrow-frame-wasm.ts', import.meta.url))
const documentPath = fileURLToPath(new URL('./arrow-frame.html', import.meta.url))
const wasmPath = createRequire(import.meta.url).resolve('@jitl/quickjs-wasmfile-release-asyncify/wasm')
const devPrefix = '/@shinona/arrow-frame/'

function replaceOnce(source: string, anchor: string, replacement: string) {
  if (!source.includes(anchor) || source.indexOf(anchor) !== source.lastIndexOf(anchor)) {
    throw new Error(`The ArrowJS sandbox integration needs updating at ${anchor}`)
  }
  return source.replace(anchor, replacement)
}

export function arrowFrame(): Plugin {
  let serve = false
  let assets: Promise<{ runtime: Uint8Array; document: Buffer; wasm: Buffer }> | undefined
  function getAssets() {
    return assets ??= Promise.all([
      bundleRuntime(),
      readFile(documentPath),
      readFile(wasmPath),
    ]).then(([runtime, document, wasm]) => {
      const compressedWasm = gzipSync(wasm)
      const size = gzipSync(runtime).length + gzipSync(document).length + compressedWasm.length
      if (size > arrowFrameGzipBudget) throw new Error(`ArrowJS frame requires ${size} compressed bytes. Budget is ${arrowFrameGzipBudget}.`)
      return { runtime, document, wasm: compressedWasm }
    }).catch((error: unknown) => {
      assets = undefined
      throw error
    })
  }
  return {
    name: 'shinona-arrow-frame',
    configResolved(config) { serve = config.command === 'serve' },
    configureServer(server) {
      server.watcher.add([entry, wasmState, documentPath])
      server.watcher.on('change', path => {
        if (path === entry || path === wasmState || path === documentPath) assets = undefined
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
      this.addWatchFile(entry)
      this.addWatchFile(wasmState)
      this.addWatchFile(documentPath)
      if (serve) {
        return `export default ${JSON.stringify({
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
      return `export default { ${exports.join(', ')} }`
    },
  }
}

async function bundleRuntime() {
  const patched = new Set<string>()
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
        plugin.onLoad({ filter: /@arrow-js[\\/]sandbox[\\/]src[\\/]host[\\/]quickjs\.ts$/ }, async ({ path }) => {
          let code = await readFile(path, 'utf8')
          // The parent supplies the bytes. Each opaque frame compiles them
          // once and creates an isolated QuickJS runtime for each view.
          code = replaceOnce(code, 'newQuickJSAsyncWASMModule(debug ? DEBUG_ASYNC : RELEASE_ASYNC)',
            'newQuickJSAsyncWASMModule(newVariant(RELEASE_ASYNC, { wasmModule: arrowWasmModule }))')
          // An uninterrupted guest evaluation or job queue must yield the UI thread.
          code = replaceOnce(code, 'runtime.setMaxStackSize(512 * 1024)',
            'runtime.setMaxStackSize(512 * 1024)\n  runtime.setInterruptHandler(() => performance.now() > (deadlines.get(runtime) ?? Infinity))')
          code = replaceOnce(code, 'function flushPendingJobs(runtime: any, context: any) {',
            'function flushPendingJobs(runtime: any, context: any) {\n  beginExecution(runtime)')
          code = replaceOnce(code, 'context.unwrapResult(runtime.executePendingJobs())',
            'if (performance.now() > deadlines.get(runtime)) throw new Error("ArrowJS execution limit exceeded")\n    context.unwrapResult(runtime.executePendingJobs(1))')
          code = replaceOnce(code, 'const result = await context.evalCodeAsync(code, fileName, { type: \'module\' })',
            'beginExecution(runtime)\n  const result = await context.evalCodeAsync(code, fileName, { type: \'module\' })')
          code = replaceOnce(code, 'const result = context.callFunction(callback, context.undefined, args)',
            'beginExecution(runtime)\n      const result = context.callFunction(callback, context.undefined, args)')
          const setup = [
            'import { newVariant } from "quickjs-emscripten"',
            `import { arrowWasmModule } from ${JSON.stringify(wasmState)}`,
            'const deadlines = new WeakMap<object, number>()',
            'function beginExecution(runtime: object) { deadlines.set(runtime, performance.now() + 750) }',
          ].join('\n')
          patched.add('execution')
          return { contents: setup + '\n' + code, loader: 'ts' }
        })
        plugin.onLoad({ filter: /@arrow-js[\\/]sandbox[\\/]src[\\/]compiler[\\/]index\.ts$/ }, async ({ path }) => {
          // The tool accepts JavaScript. Acorn still checks its syntax and extracts templates.
          let code = await readFile(path, 'utf8')
          code = replaceOnce(code, "import ts from 'typescript'\n", '')
          const contents = replaceOnce(code, '    const emitted = transpileModule(path, source)\n    const processed = preprocessModule(emitted, path)',
            '    const processed = preprocessModule(source, path)')
          patched.add('compiler')
          return { contents, loader: 'ts' }
        })
        plugin.onLoad({ filter: /@arrow-js[\\/]sandbox[\\/]src[\\/]compiler[\\/]template\.ts$/ }, async ({ path }) => {
          const code = await readFile(path, 'utf8')
          // Active document elements can navigate their own frame even with a restrictive CSP.
          const contents = replaceOnce(code, 'function compileElementNode(element: Element): ElementDescriptor {',
            'function compileElementNode(element: Element): ElementDescriptor {\n' +
            '  if (/^(?:base|embed|frame|iframe|link|meta|object|script)$/i.test(element.localName)) {\n' +
            '    throw new SandboxCompileError("Unsupported active document element")\n  }')
          patched.add('elements')
          return { contents, loader: 'ts' }
        })
      },
    }],
  })
  if (patched.size !== 3 || !bundle.outputFiles[0]) throw new Error('The ArrowJS sandbox build is missing required integration changes')
  return bundle.outputFiles[0].contents
}
