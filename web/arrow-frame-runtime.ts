import { preprocessModule } from 'virtual:arrow-compiler'
import type { SerializedNode, VmPatch, VmToHostMessage } from 'virtual:arrow-protocol'
import { HostRenderer } from 'virtual:arrow-renderer'
import { createVmRunner } from 'virtual:arrow-vm'
import type { VmRunner } from 'virtual:arrow-vm'

import { initializeArrowWasm } from './arrow-frame-quickjs'

interface FrameInput {
  type: 'render'
  source: Record<string, string>
  theme: Record<string, string>
  lang: string
  wasm: ArrayBuffer
}

const root = document.body
const activeElements = /^(?:base|embed|fencedframe|frame|iframe|link|meta|object|portal|script)$/i

function assertPassive(nodes: SerializedNode[]) {
  for (const node of nodes) {
    if (node.kind === 'element') {
      const tag = node.tag
      if (typeof tag !== 'string' || activeElements.test(tag.slice(tag.lastIndexOf(':') + 1))) {
        throw new Error('Unsupported active document element.')
      }
    }
    if ('children' in node) assertPassive(node.children)
  }
}

function preventNavigation(event: Event) {
  if (event.composedPath().some(node => node instanceof Element && ['a', 'area'].includes(node.localName))) {
    event.preventDefault()
  }
}

function onMessage(event: MessageEvent<FrameInput>) {
  const port = event.ports[0]
  if (event.source !== window.parent || event.data?.type !== 'render' || !port) return
  window.removeEventListener('message', onMessage)
  void render(event.data, port)
}
window.addEventListener('message', onMessage)

async function render({ source, theme, lang, wasm }: FrameInput, port: MessagePort) {
  initializeArrowWasm(wasm)
  document.documentElement.lang = lang
  for (const [name, value] of Object.entries(theme)) root.style.setProperty(name, value)

  const host = document.createElement('arrow-sandbox')
  const surface = host.attachShadow({ mode: 'open' })
  const style = document.createElement('style')
  style.textContent = ':where(table) { width: 100%; }\n' + (source['main.css'] ?? '')
  const mountPoint = document.createElement('div')
  surface.append(style, mountPoint)
  root.append(host)

  const lifetime = new AbortController()
  const { signal } = lifetime
  for (const name of ['click', 'auxclick']) root.addEventListener(name, preventNavigation, { capture: true, signal })
  let runner: VmRunner | undefined
  let initialTree: SerializedNode | undefined
  const initialPatches: VmPatch[] = []
  let failed = false
  const fail = () => {
    if (failed || signal.aborted) return
    failed = true
    port.postMessage({ type: 'error' })
  }
  const renderer = new HostRenderer({
    mountPoint,
    async onEvent(handlerId, event) {
      if (!failed) await runner?.dispatch({ type: 'event', payload: { handlerId, event } })
    },
    onError: fail,
  })

  window.addEventListener('pagehide', () => {
    lifetime.abort()
    port.close()
    renderer.destroy()
    runner?.destroy()
  }, { once: true })

  function receive(message: VmToHostMessage) {
    if (signal.aborted || failed) return
    try {
      switch (message.type) {
        case 'render':
          assertPassive([message.tree])
          if (runner) renderer.render(message.tree)
          else initialTree = message.tree
          break
        case 'patch':
          for (const patch of message.patches) {
            if (patch.type === 'replace-region') assertPassive(patch.children)
          }
          if (runner) renderer.applyPatches(message.patches)
          else initialPatches.push(...message.patches)
          break
        case 'error':
          fail()
      }
    } catch {
      fail()
    }
  }
  try {
    const script = source['main.js']
    if (!script) throw new Error('Missing ArrowJS source.')
    const entryPath = '/main.js'
    const { code, descriptors } = preprocessModule(script, entryPath)
    const boot = await createVmRunner({
      compiled: {
        entryPath,
        modules: { [entryPath]: code },
        descriptors: Object.fromEntries(descriptors.map(descriptor => [descriptor.id, descriptor])),
      },
      onMessage: receive,
    })
    if (signal.aborted || failed) {
      boot.destroy()
      return
    }
    runner = boot
    if (!initialTree) throw new Error('The ArrowJS view did not render.')
    renderer.render(initialTree)
    renderer.applyPatches(initialPatches)
    initialTree = undefined
    initialPatches.length = 0
    reportHeight(host, surface, port, signal)
  } catch {
    fail()
  }
}

function reportHeight(host: Element, surface: ShadowRoot, port: MessagePort, signal: AbortSignal) {
  let lastHeight = 0
  const report = () => {
    const bounds = host.getBoundingClientRect()
    const overflowBottom = host.scrollHeight > host.clientHeight
      ? bounds.top + host.clientTop + host.scrollHeight + 1
      : bounds.bottom
    const contentBottom = Math.max(bounds.bottom, overflowBottom) + window.scrollY
    const horizontalScrollbar = window.innerHeight - document.documentElement.clientHeight
    const height = Math.max(1, Math.ceil(contentBottom) + horizontalScrollbar)
    if (height === lastHeight) return
    lastHeight = height
    port.postMessage({ type: 'resize', height })
  }
  const resize = new ResizeObserver(report)
  resize.observe(host)
  const mutations = new MutationObserver(report)
  mutations.observe(surface, { attributes: true, characterData: true, childList: true, subtree: true })
  surface.addEventListener('load', report, { capture: true, signal })
  window.addEventListener('resize', report, { signal })
  signal.addEventListener('abort', () => {
    resize.disconnect()
    mutations.disconnect()
  }, { once: true })
  report()
}
