let compiled: Promise<WebAssembly.Module> | undefined

export function initializeArrowWasm(bytes: ArrayBuffer | undefined) {
  if (bytes && !compiled) {
    const pending = WebAssembly.compile(bytes)
    compiled = pending
    void pending.catch(() => {
      if (compiled === pending) compiled = undefined
    })
  }
}

export function arrowWasmModule(): Promise<WebAssembly.Module> {
  if (!compiled) throw new Error('Missing ArrowJS WebAssembly module.')
  return compiled
}
