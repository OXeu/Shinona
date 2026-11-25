import { sandbox } from '@arrow-js/sandbox'

interface FrameInput {
  source: Record<string, string>
  theme: Record<string, string>
  lang: string
}

window.addEventListener('message', function start(event: MessageEvent<FrameInput>) {
  const port = event.ports[0]
  if (event.source !== window.parent || !port) return
  window.removeEventListener('message', start)
  const { source, theme, lang } = event.data
  const root = document.body
  document.documentElement.lang = lang
  for (const [name, value] of Object.entries(theme)) root.style.setProperty(name, value)

  // Arrow templates support HTML and SVG links. Keep navigation inside the app out of the view.
  function preventNavigation(event: Event) {
    if (event.composedPath().some(node => node instanceof Element && ['a', 'area'].includes(node.localName))) {
      event.preventDefault()
    }
  }
  for (const name of ['click', 'auxclick']) root.addEventListener(name, preventNavigation, true)

  const fail = () => {
    port.postMessage({ type: 'error' })
  }
  try {
    sandbox({ source, onError: fail })(root)
    const host = root.querySelector('arrow-sandbox')
    if (!host) {
      fail()
      return
    }
    host.addEventListener('sandbox-ready', () => {
      const content = host.shadowRoot ?? host
      const defaults = document.createElement('style')
      defaults.textContent = ':where(table) { width: 100%; }'
      content.prepend(defaults)

      let lastHeight = 0
      const report = () => {
        const bounds = host.getBoundingClientRect()
        const overflowBottom = host.scrollHeight > host.clientHeight
          ? bounds.top + host.clientTop + host.scrollHeight + 1 // Rounded scrollHeight can miss fractional overflow.
          : bounds.bottom
        const contentBottom = Math.max(bounds.bottom, overflowBottom) + window.scrollY
        const horizontalScrollbar = window.innerHeight - document.documentElement.clientHeight
        const height = Math.max(1, Math.ceil(contentBottom) + horizontalScrollbar)
        if (height === lastHeight) return
        lastHeight = height
        port.postMessage({ type: 'resize', height })
      }
      new ResizeObserver(report).observe(host)
      new MutationObserver(report).observe(content, { attributes: true, characterData: true, childList: true, subtree: true })
      content.addEventListener('load', report, true)
      window.addEventListener('resize', report)
      report()
    }, { once: true })
  } catch {
    fail()
  }
})
