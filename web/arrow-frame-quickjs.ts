import {
  DisposableSuccess, newQuickJSAsyncWASMModule as loadQuickJS, newVariant, RELEASE_ASYNC,
} from 'quickjs-emscripten'
import type { AsyncRuntimeOptions, QuickJSAsyncRuntime } from 'quickjs-emscripten'

// The iframe only loads the release WASM supplied by the parent.
export { RELEASE_ASYNC, RELEASE_ASYNC as DEBUG_ASYNC }

const executionLimitMS = 750

function limitExecution(runtime: QuickJSAsyncRuntime) {
  let deadline = Infinity
  const start = () => { deadline = performance.now() + executionLimitMS }
  const expired = () => performance.now() >= deadline
  runtime.setInterruptHandler(expired)

  const newContext = runtime.newContext.bind(runtime)
  runtime.newContext = options => {
    const context = newContext(options)
    const evaluate = context.evalCodeAsync.bind(context)
    context.evalCodeAsync = (...args) => {
      start()
      return evaluate(...args)
    }
    const call = context.callFunction.bind(context)
    const guardedCall: typeof context.callFunction = (fn, receiver, ...args) => {
      start()
      // QuickJS accepts either an argument array or individual handles.
      return call(fn, receiver, ...args.flat().filter(handle => handle !== undefined))
    }
    context.callFunction = guardedCall
    return context
  }

  const execute = runtime.executePendingJobs.bind(runtime)
  runtime.executePendingJobs = (limit = -1) => {
    start()
    const maxJobs = limit < 0 ? Infinity : limit
    let completed = 0
    while (completed < maxJobs && runtime.hasPendingJob()) {
      if (expired()) throw new Error('ArrowJS execution limit exceeded.')
      const result = execute(1)
      if (result.error) return result
      if (!result.value) break
      completed += result.value
    }
    return new DisposableSuccess(completed)
  }
  return runtime
}

let wasm: ArrayBuffer | undefined

export function initializeArrowWasm(bytes: ArrayBuffer) {
  wasm = bytes
}

export async function newQuickJSAsyncWASMModule() {
  const bytes = wasm
  wasm = undefined
  if (!bytes) throw new Error('Missing ArrowJS WebAssembly module.')
  const quickJS = await loadQuickJS(newVariant(RELEASE_ASYNC, { wasmModule: () => WebAssembly.compile(bytes) }))
  return {
    newRuntime(options?: AsyncRuntimeOptions) {
      return limitExecution(quickJS.newRuntime(options))
    },
  }
}
