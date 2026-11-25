const maxFrameBytes = 64 * 1024 * 1024

// HTTP chunk boundaries need not coincide with the length-prefixed Protobuf frames.
export async function* transcriptFrames(body: ReadableStream<Uint8Array>) {
  const reader = body.getReader()
  let source: Uint8Array = new Uint8Array(0)
  let offset = 0
  async function take(size: number, allowEnd = false) {
    const bytes = new Uint8Array(size)
    let filled = 0
    while (filled < size) {
      if (offset === source.byteLength) {
        const { done, value } = await reader.read()
        if (done) {
          if (allowEnd && filled === 0) return
          throw new Error('The transcript frame ended early.')
        }
        source = value
        offset = 0
      }
      const length = Math.min(source.byteLength - offset, size - filled)
      bytes.set(source.subarray(offset, offset + length), filled)
      offset += length
      filled += length
    }
    return bytes
  }
  try {
    let header
    while ((header = await take(4, true))) {
      const size = new DataView(header.buffer).getUint32(0)
      if (size === 0 || size > maxFrameBytes) throw new Error('Invalid transcript frame length.')
      const frame = await take(size)
      if (!frame) throw new Error('Missing transcript frame.')
      yield frame
    }
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}
