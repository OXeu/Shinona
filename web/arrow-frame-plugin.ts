import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { build } from 'esbuild'
import type { Plugin } from 'vite'

const id = 'virtual:arrow-frame'
const entry = fileURLToPath(new URL('./arrow-frame-runtime.ts', import.meta.url))

function replaceOnce(source: string, anchor: string, replacement: string) {
  if (!source.includes(anchor) || source.indexOf(anchor) !== source.lastIndexOf(anchor)) {
    throw new Error(`The ArrowJS sandbox integration needs updating at ${anchor}`)
  }
  return source.replace(anchor, replacement)
}

export function arrowFrame(): Plugin {
  return {
    name: 'shinona-arrow-frame',
    resolveId: source => source === id ? '\0' + id : null,
    async load(source) {
      if (source !== '\0' + id) return
      this.addWatchFile(entry)
      const patched = new Set<string>()
      const bundle = await build({
        entryPoints: [entry],
        format: 'esm',
        bundle: true,
        minify: true,
        write: false,
        target: 'es2023',
        loader: { '.wasm': 'binary' },
        plugins: [{
          name: 'isolated-arrow-runtime',
          setup(plugin) {
            plugin.onLoad({ filter: /@arrow-js[\\/]sandbox[\\/]src[\\/]host[\\/]quickjs\.ts$/ }, async ({ path }) => {
              let code = await readFile(path, 'utf8')
              // Embed WASM so the opaque-origin frame can run with networking disabled.
              code = replaceOnce(code, 'newQuickJSAsyncWASMModule(debug ? DEBUG_ASYNC : RELEASE_ASYNC)',
                'newQuickJSAsyncWASMModule(newVariant(RELEASE_ASYNC, { wasmBinary: wasm.buffer }))')
              // An uninterrupted guest evaluation or job queue must yield the UI thread.
              code = replaceOnce(code, 'runtime.setMaxStackSize(512 * 1024)',
                'runtime.setMaxStackSize(512 * 1024)\n  runtime.setInterruptHandler(() => performance.now() > deadline)')
              code = replaceOnce(code, 'context.unwrapResult(runtime.executePendingJobs())',
                'if (performance.now() > deadline) throw new Error("ArrowJS execution limit exceeded")\n    context.unwrapResult(runtime.executePendingJobs(1))')
              const setup = [
                'import { newVariant } from "quickjs-emscripten"',
                'import wasm from "@jitl/quickjs-wasmfile-release-asyncify/wasm"',
                'let deadline = performance.now() + 750',
                'setInterval(() => { deadline = performance.now() + 750 }, 100)',
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
      if (patched.size !== 3) throw new Error('The ArrowJS sandbox build is missing required integration changes')
      const runtime = bundle.outputFiles[0].text.replace(/<\/script/giu, '<\\/script')
      return `export default ${JSON.stringify(runtime)}`
    },
  }
}
