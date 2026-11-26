let compiled: Promise<WebAssembly.Module> | undefined

export function initializeArrowWasm(bytes: ArrayBuffer | undefined) {
  if (bytes && !compiled) compiled = WebAssembly.compile(bytes)
}

export function arrowWasmModule(): Promise<WebAssembly.Module> {
  if (!compiled) throw new Error('Missing ArrowJS WebAssembly module.')
  return compiled
}
