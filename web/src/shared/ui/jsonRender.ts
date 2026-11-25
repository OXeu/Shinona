import { defineCatalog } from '@json-render/core'
import type { Spec } from '@json-render/core'
import { defineRegistry } from '@json-render/vue'
import { schema } from '@json-render/vue/schema'
import { h } from 'vue'
import { z } from 'zod'

const shortText = z.string().min(1).refine(value => Array.from(value).length <= 160)
const text = z.string().refine(value => Array.from(value).length <= 1200)
const props = {
  Stack: z.strictObject({}),
  Card: z.strictObject({ title: shortText, description: text.optional() }),
  Text: z.strictObject({ text }),
  Metric: z.strictObject({ label: shortText, value: shortText, detail: text.optional() }),
  Badge: z.strictObject({ text: shortText, tone: z.enum(['neutral', 'success', 'warning', 'danger']).optional() }),
  List: z.strictObject({ items: z.array(text).max(24) }),
  Table: z.strictObject({ columns: z.array(shortText).min(1).max(8), rows: z.array(z.array(text).max(8)).max(24) }),
  Progress: z.strictObject({ label: shortText, value: z.number().min(0).max(100) }),
}

const catalog = defineCatalog(schema, {
  components: {
    Stack: { props: props.Stack, slots: ['default'], description: 'Vertical stack' },
    Card: { props: props.Card, slots: ['default'], description: 'Titled group' },
    Text: { props: props.Text, description: 'Plain text' },
    Metric: { props: props.Metric, description: 'Label and value' },
    Badge: { props: props.Badge, description: 'Status badge' },
    List: { props: props.List, description: 'Bulleted list' },
    Table: { props: props.Table, description: 'Data table' },
    Progress: { props: props.Progress, description: 'Progress from 0 to 100' },
  },
  actions: {},
})

export const { registry } = defineRegistry(catalog, {
  components: {
    Stack: ({ children }) => h('div', { class: 'rich-stack' }, children),
    Card: ({ props, children }) => h('section', { class: 'rich-card' }, [
      h('h3', props.title),
      props.description ? h('p', { class: 'rich-detail' }, props.description) : null,
      children,
    ]),
    Text: ({ props }) => h('p', { class: 'rich-text' }, props.text),
    Metric: ({ props }) => h('div', { class: 'rich-metric' }, [
      h('span', { class: 'rich-detail' }, props.label),
      h('strong', props.value),
      props.detail ? h('span', { class: 'rich-detail' }, props.detail) : null,
    ]),
    Badge: ({ props }) => h('span', { class: 'rich-badge', 'data-tone': props.tone }, props.text),
    List: ({ props }) => h('ul', { class: 'rich-list' }, props.items.map(item => h('li', item))),
    Table: ({ props }) => h('div', { class: 'rich-table-scroll' }, [h('table', { class: 'rich-table' }, [
      h('thead', [h('tr', props.columns.map(label => h('th', { scope: 'col' }, label)))]),
      h('tbody', props.rows.map(row => h('tr', props.columns.map((_, index) => h('td', row[index] ?? ''))))),
    ])]),
    Progress: ({ props }) => h('div', { class: 'rich-progress' }, [
      h('span', props.label),
      h('span', { class: 'rich-detail' }, `${props.value}%`),
      h('progress', { 'aria-label': props.label, max: 100, value: props.value }),
    ]),
  },
})

const id = z.string().regex(/^[A-Za-z0-9_-]{1,48}$/u)
  .refine(value => !['__proto__', 'constructor', 'prototype'].includes(value))
const type = z.custom<keyof typeof props>(value => typeof value === 'string' && Object.hasOwn(props, value))
const inputSchema = z.strictObject({
  root: id,
  elements: z.record(id, z.strictObject({ type, props: z.record(z.string(), z.unknown()), children: z.array(id).max(40).optional() })),
})

export function parseDisplaySpec(raw: string): Spec | null {
  if (new TextEncoder().encode(raw).length > 8192) return null
  let input: unknown
  try {
    input = JSON.parse(raw)
  } catch {
    return null
  }
  const result = inputSchema.safeParse(input)
  if (!result.success) return null
  const { root, elements } = result.data
  const size = Object.keys(elements).length
  if (size > 40) return null
  const visited = new Set<string>()
  function visit(key: string, depth: number): boolean {
    const element = Object.hasOwn(elements, key) ? elements[key] : undefined
    if (!element || depth > 12 || visited.has(key) || !props[element.type].safeParse(element.props).success) return false
    if (element.children?.length && element.type !== 'Stack' && element.type !== 'Card') return false
    visited.add(key)
    return element.children?.every(child => visit(child, depth + 1)) ?? true
  }
  return visit(root, 1) && visited.size === size ? result.data : null
}
