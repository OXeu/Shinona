import { html, reactive } from '@arrow-js/core'
import type { ArrowTemplate } from '@arrow-js/core'
import { sandbox } from '@arrow-js/sandbox'

import { initializeArrowWasm } from './arrow-frame-wasm'

interface FrameInput {
  type: 'render'
  source: Record<string, string>
  theme: Record<string, string>
  lang: string
  wasm?: ArrayBuffer
}

const root = document.body
const state = reactive<{ view: ArrowTemplate | undefined }>({ view: undefined })
html`${() => state.view ?? ''}`(root)

function preventNavigation(event: Event) {
  if (event.composedPath().some(node => node instanceof Element && ['a', 'area'].includes(node.localName))) {
    event.preventDefault()
  }
}
for (const name of ['click', 'auxclick']) root.addEventListener(name, preventNavigation, true)

let closeView: (() => void) | undefined
window.addEventListener('message', (event: MessageEvent<FrameInput | { type: 'clear' }>) => {
  if (event.source !== window.parent) return
  const input = event.data
  if (input?.type === 'clear') {
    closeView?.()
    return
  }
  const port = event.ports[0]
  if (input?.type !== 'render' || !port) return
  closeView?.()
  closeView = render(input, port)
})

function render({ source, theme, lang, wasm }: FrameInput, port: MessagePort) {
  initializeArrowWasm(wasm)
  document.documentElement.lang = lang
  root.removeAttribute('style')
  for (const [name, value] of Object.entries(theme)) root.style.setProperty(name, value)

  const listeners = new AbortController()
  const { signal } = listeners
  let resize: ResizeObserver | undefined
  let mutations: MutationObserver | undefined
  const fail = () => {
    if (!signal.aborted) port.postMessage({ type: 'error' })
  }
  const mount = new MutationObserver(() => {
    const host = root.querySelector('arrow-sandbox')
    if (!host) return
    mount.disconnect()
    host.addEventListener('sandbox-error', fail, { signal })
    const observe = () => {
      const content = host.shadowRoot ?? host
      const defaults = document.createElement('style')
      defaults.textContent = ':where(table) { width: 100%; }'
      content.prepend(defaults)

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
      resize = new ResizeObserver(report)
      resize.observe(host)
      mutations = new MutationObserver(report)
      mutations.observe(content, { attributes: true, characterData: true, childList: true, subtree: true })
      content.addEventListener('load', report, { capture: true, signal })
      window.addEventListener('resize', report, { signal })
      report()
    }
    if (host.getAttribute('data-ready') === 'true') observe()
    else host.addEventListener('sandbox-ready', observe, { once: true, signal })
  })
  mount.observe(root, { childList: true, subtree: true })
  try {
    state.view = sandbox({ source, onError: fail })
  } catch {
    fail()
  }

  return () => {
    listeners.abort()
    mount.disconnect()
    resize?.disconnect()
    mutations?.disconnect()
    state.view = undefined
    root.removeAttribute('style')
    port.close()
    closeView = undefined
  }
}
