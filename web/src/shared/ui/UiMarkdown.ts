import MarkdownIt from 'markdown-it'
import { defineComponent, h } from 'vue'

// Keep model-provided HTML inert and prevent automatic requests to image URLs.
const markdown = new MarkdownIt({ html: false }).disable('image')

export default defineComponent({
  props: { text: { type: String, required: true } },
  setup(props) {
    return () => h('div', { class: 'ui-markdown', innerHTML: markdown.render(props.text) })
  },
})
