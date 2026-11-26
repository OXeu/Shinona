import assets from 'virtual:arrow-frame'
import { request } from '../http'

const maxFrames = 3
const idleTimeout = 20_000
const frames = new Set<Frame>()
const waiting = new Set<() => void>()
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
}

function loadSources() {
  return sources ??= Promise.all([
    request(assets.document).then(response => response.text()),
    request(assets.wasm).then(response => {
      // Some static servers mark .gz assets as encoded, so fetch decompresses them.
      if (response.headers.get('Content-Encoding') === 'gzip') return response.arrayBuffer()
      if (!response.body) throw new Error('Missing ArrowJS WebAssembly response.')
      return new Response(response.body.pipeThrough(new DecompressionStream('gzip'))).arrayBuffer()
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
  frame.element.remove()
  frame.readyNow()
  frames.delete(frame)
}

function lease(frame: Frame, wasm: ArrayBuffer) {
  function release(reuse: boolean) {
    const element = frame.element
    const slot = element.parentElement as FrameSlot | null
    if (reuse && slot?.moveBefore && element.isConnected && frame.initialized) {
      if (!parking) {
        parking = document.createElement('div')
        parking.hidden = true
        document.body.append(parking)
      }
      element.contentWindow?.postMessage({ type: 'clear' }, '*')
      const destination: FrameSlot = parking
      destination.moveBefore?.(element, null)
      frame.busy = false
      frame.timer = window.setTimeout(() => discard(frame), idleTimeout)
    } else {
      discard(frame)
    }
    waiting.values().next().value?.()
  }
  return {
    element: frame.element,
    ready: frame.ready,
    render(input: { source: Record<string, string>; theme: Record<string, string>; lang: string }, port: MessagePort) {
      frame.element.contentWindow?.postMessage({ ...input, type: 'render', wasm: frame.initialized ? undefined : wasm }, '*', [port])
      frame.initialized = true
    },
    release,
  }
}

export async function borrowArrowFrame(slot: FrameSlot, title: string, signal: AbortSignal) {
  const [source, wasm] = await loadSources()
  while (!signal.aborted) {
    const idle = [...frames].find(frame => !frame.busy)
    if (idle && slot.moveBefore) {
      window.clearTimeout(idle.timer)
      idle.busy = true
      idle.element.title = title
      slot.moveBefore(idle.element, null)
      return lease(idle, wasm)
    }
    if (idle) {
      window.clearTimeout(idle.timer)
      discard(idle)
    }
    if (frames.size < maxFrames) {
      const frame = createFrame(source)
      frame.element.title = title
      slot.append(frame.element)
      return lease(frame, wasm)
    }
    await new Promise<void>(resolve => {
      function wake() {
        waiting.delete(wake)
        signal.removeEventListener('abort', wake)
        resolve()
      }
      signal.addEventListener('abort', wake, { once: true })
      waiting.add(wake)
    })
  }
}
