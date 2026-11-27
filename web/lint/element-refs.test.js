import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createRenderer, h, nextTick, ref } from 'vue'

import { UiBox } from '../src/shared/ui/elements.ts'

test('native element refs only notify on mount and unmount across reactive renders', async t => {
  class Element {}
  const node = new Element()
  Object.defineProperty(globalThis, 'HTMLElement', { configurable: true, value: Element })
  t.after(() => Reflect.deleteProperty(globalThis, 'HTMLElement'))
  const renderer = createRenderer({
    createElement: () => node,
    insert(element, parent) {
      element.parent = parent
    },
    parentNode: element => element.parent,
    nextSibling: () => null,
    patchProp(element, key, _, value) {
      element[key] = value
    },
    remove() {},
  })
  const value = ref(0)
  const elementRef = t.mock.fn()
  const app = renderer.createApp({
    render: () => h(UiBox, { elementRef, 'data-value': value.value }),
  })
  app.mount(new Element())
  assert.deepEqual(elementRef.mock.calls.map(call => call.arguments), [[node]])
  value.value++
  await nextTick()
  assert.equal(node['data-value'], 1)
  assert.equal(elementRef.mock.callCount(), 1)
  app.unmount()
  assert.deepEqual(elementRef.mock.calls.map(call => call.arguments), [[node], [null]])
})
