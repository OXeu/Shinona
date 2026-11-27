import { computed, defineComponent, h, mergeProps } from 'vue'
import type { PropType } from 'vue'

function element(tag: keyof HTMLElementTagNameMap, defaults: Record<string, string> = {}) {
  return defineComponent({
    inheritAttrs: false,
    props: {
      elementRef: {
        type: Function as PropType<(element: HTMLElement | null) => void>,
        default: undefined,
      },
    },
    setup(props, { attrs, slots }) {
      const elementRef = computed(() => {
        const callback = props.elementRef
        if (!callback) return
        let previous: HTMLElement | null = null
        return (value: unknown) => {
          const element = value instanceof HTMLElement ? value : null
          if (element === previous) return
          previous = element
          callback(element)
        }
      })
      return () => {
        const bindings = mergeProps(defaults, attrs)
        if (elementRef.value) bindings.ref = elementRef.value
        return h(tag, bindings, slots.default?.())
      }
    },
  })
}

export const UiBox = element('div')
export const UiMain = element('main')
export const UiHeader = element('header')
export const UiNav = element('nav')
export const UiSection = element('section')
export const UiHeading = element('h1')
export const UiSubheading = element('h2')
export const UiInline = element('span')
export const UiPre = element('pre')
export const UiForm = element('form', { novalidate: '', class: 'ui-form' })
export const UiList = element('ul')
export const UiListItem = element('li')
export const UiAnchor = element('a')
export const UiButton = element('button', { type: 'button' })
export const UiTime = element('time')
export const UiImage = element('img')
