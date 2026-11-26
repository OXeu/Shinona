import assets from 'virtual:arrow-frame'
import { request } from '../http'
import { ArrowFrameSlots } from './arrowFrameSlots'

const slots = new ArrowFrameSlots()
let sources: Promise<[string, ArrayBuffer]> | undefined

function loadSources() {
  if (sources) return sources
  const signal = AbortSignal.timeout(10_000)
  sources = Promise.all([
    request(assets.document, undefined, signal).then(response => response.text()),
    request(assets.wasm, undefined, signal).then(async response => {
      const bytes = await response.arrayBuffer()
      const header = new Uint8Array(bytes, 0, Math.min(4, bytes.byteLength))
      // Static servers may send the asset as raw gzip or decode it at fetch.
      if (header[0] === 0 && header[1] === 0x61 && header[2] === 0x73 && header[3] === 0x6d) return bytes
      if (header[0] !== 0x1f || header[1] !== 0x8b) throw new Error('Invalid ArrowJS WebAssembly response.')
      const body = new Blob([bytes]).stream().pipeThrough(new DecompressionStream('gzip'))
      return new Response(body).arrayBuffer()
    }),
  ]).catch((error: unknown) => {
    sources = undefined
    throw error
  })
  return sources
}

interface FrameRequest {
  slot: HTMLElement
  title: string
  signal: AbortSignal
  onWait: () => void
  onRevoke: () => void
  priority: boolean
}

export async function mountArrowFrame({ slot, title, signal, onWait, onRevoke, priority }: FrameRequest) {
  const [source, wasm] = await loadSources()
  const releaseSlot = await slots.reserve(signal, onWait, onRevoke, priority)
  if (!releaseSlot || signal.aborted) {
    releaseSlot?.()
    return
  }

  const element = document.createElement('iframe')
  let markReady = () => {}
  const ready = new Promise<void>(resolve => { markReady = resolve })
  let disposed = false
  function release() {
    if (disposed) return
    disposed = true
    signal.removeEventListener('abort', release)
    element.remove()
    markReady()
    releaseSlot?.()
  }
  try {
    const entropy = crypto.getRandomValues(new Uint8Array(16))
    const nonce = Array.from(entropy, byte => byte.toString(16).padStart(2, '0')).join('')
    element.className = 'arrow-view'
    element.title = title
    element.sandbox.add('allow-scripts')
    element.referrerPolicy = 'no-referrer'
    const runtime = new URL(assets.runtime, document.baseURI).href.replaceAll('&', '&amp;').replaceAll('"', '&quot;')
    element.srcdoc = source.replaceAll('ARROW_FRAME_NONCE', nonce).replace('ARROW_FRAME_RUNTIME', () => runtime)
    element.addEventListener('load', markReady, { once: true, signal })
    signal.addEventListener('abort', release, { once: true })
    slot.append(element)
  } catch (error) {
    release()
    throw error
  }

  return {
    element,
    ready,
    render(input: { source: Record<string, string>; theme: Record<string, string>; lang: string }, port: MessagePort) {
      const target = element.contentWindow
      try {
        if (disposed || !target) throw new Error('The ArrowJS frame is unavailable.')
        target.postMessage({ ...input, type: 'render', wasm }, '*', [port])
      } catch (error) {
        port.close()
        throw error
      }
    },
    release,
  }
}
