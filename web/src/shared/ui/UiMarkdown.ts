import MarkdownIt from 'markdown-it'
import { defineComponent, h } from 'vue'
import type { VNode } from 'vue'

// Keep model-provided HTML inert and prevent automatic requests to image URLs.
const markdown = new MarkdownIt({ html: false }).disable('image')
markdown.validateLink = (raw) => {
  try {
    const url = new URL(raw)
    return (url.protocol === 'https:' || url.protocol === 'http:') && !url.username && !url.password
  } catch {
    return false
  }
}

type Token = ReturnType<typeof markdown.parse>[number]
type Child = VNode | string

function render(tokens: Token[]): Child[] {
  const root: Child[] = []
  const stack: { token: Token; children: Child[] }[] = []
  const children = () => stack.at(-1)?.children ?? root
  for (const token of tokens) {
    if (token.hidden) continue
    if (token.nesting === 1) {
      stack.push({ token, children: [] })
    } else if (token.nesting === -1) {
      const frame = stack.pop()
      if (!frame) continue
      const attrs = Object.fromEntries(frame.token.attrs ?? [])
      if (frame.token.type === 'link_open') {
        Object.assign(attrs, { target: '_blank', rel: 'noopener noreferrer' })
      }
      children().push(h(frame.token.tag, attrs, frame.children))
    } else if (token.children) {
      for (const child of render(token.children)) children().push(child)
    } else if (token.type === 'fence' || token.type === 'code_block') {
      const language = token.info.trim().split(/\s+/, 1)[0]
      const attrs = language ? { class: 'language-' + language } : {}
      children().push(h('pre', [h('code', attrs, token.content)]))
    } else if (token.tag) {
      children().push(h(token.tag, token.content))
    } else if (token.type === 'softbreak') {
      children().push('\n')
    } else {
      children().push(token.content)
    }
  }
  return root
}

export default defineComponent({
  props: { text: { type: String, required: true } },
  setup(props) {
    return () => h('div', { class: 'ui-markdown' }, render(markdown.parse(props.text, {})))
  },
})
