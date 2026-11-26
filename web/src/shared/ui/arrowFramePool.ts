import assets from 'virtual:arrow-frame'
import { request } from '../http'

const maxFrames = 3
const idleTimeout = 20_000
const frames = new Set<Frame>()
let parking: HTMLDivElement | undefined
let sources: Promise<[string, ArrayBuffer]> | undefined

interface FrameSlot extends HTMLElement {
  moveBefore?: (node: Node, anchor: Node | null) => void
}

interface Frame {
  element: HTMLIFrameElement
  ready: Promise<void>
  readyNow: () => void
  initialized: boolean
  busy: boolean
  timer?: number
  revoke?: () => void
}

function loadSources() {
  return sources ??= Promise.all([
    request(assets.document).then(response => response.text()),
    request(assets.wasm).then(async response => {
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
}

function createFrame(source: string) {
  const nonce = Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')
  const element = document.createElement('iframe')
  element.className = 'arrow-view'
  element.sandbox.add('allow-scripts')
  element.referrerPolicy = 'no-referrer'
  const runtime = new URL(assets.runtime, document.baseURI).href.replaceAll('&', '&amp;').replaceAll('"', '&quot;')
  element.srcdoc = source.replaceAll('ARROW_FRAME_NONCE', nonce).replace('ARROW_FRAME_RUNTIME', runtime)
  let readyNow = () => {}
  const ready = new Promise<void>(resolve => { readyNow = resolve })
  element.addEventListener('load', () => readyNow(), { once: true })
  const frame = { element, ready, readyNow, initialized: false, busy: true }
  frames.add(frame)
  return frame
}

function discard(frame: Frame) {
  window.clearTimeout(frame.timer)
  frame.revoke = undefined
  frame.element.remove()
  frame.readyNow()
  frames.delete(frame)
}

function lease(frame: Frame, wasm: ArrayBuffer) {
  let released = false
  function release(reuse: boolean) {
    if (released) return
    released = true
    const element = frame.element
    const slot = element.parentElement as FrameSlot | null
    const target = element.contentWindow
    if (reuse && slot?.moveBefore && element.isConnected && frame.initialized && target) {
      if (!parking) {
        parking = document.createElement('div')
        parking.hidden = true
        document.body.append(parking)
      }
      target.postMessage({ type: 'clear' }, '*')
      const destination: FrameSlot = parking
      destination.moveBefore?.(element, null)
      frame.busy = false
      frame.revoke = undefined
      frame.timer = window.setTimeout(() => discard(frame), idleTimeout)
    } else {
      discard(frame)
    }
  }
  return {
    element: frame.element,
    ready: frame.ready,
    render(input: { source: Record<string, string>; theme: Record<string, string>; lang: string }, port: MessagePort) {
      const target = frame.element.contentWindow
      if (released || !target) {
        port.close()
        throw new Error('The ArrowJS frame is unavailable.')
      }
      target.postMessage({ ...input, type: 'render', wasm: frame.initialized ? undefined : wasm }, '*', [port])
      frame.initialized = true
    },
    release,
  }
}

export async function borrowArrowFrame(slot: FrameSlot, title: string, signal: AbortSignal, revoke: () => void, replace: boolean) {
  const [source, wasm] = await loadSources()
  if (signal.aborted) return
  let idle = [...frames].find(frame => !frame.busy)
  if (!idle && frames.size >= maxFrames) {
    if (!replace) return
    frames.values().next().value?.revoke?.()
    idle = [...frames].find(frame => !frame.busy)
  }
  let frame: Frame
  if (idle && slot.moveBefore) {
    window.clearTimeout(idle.timer)
    frames.delete(idle)
    frames.add(idle)
    frame = idle
    slot.moveBefore(frame.element, null)
  } else {
    if (idle) discard(idle)
    if (frames.size >= maxFrames) return
    frame = createFrame(source)
    slot.append(frame.element)
  }
  frame.busy = true
  frame.element.title = title
  frame.revoke = revoke
  return lease(frame, wasm)
}
