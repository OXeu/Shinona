import { defineComponent, h } from 'vue'

function element(tag: keyof HTMLElementTagNameMap, defaults: Record<string, string> = {}) {
  return defineComponent({
    inheritAttrs: false,
    setup(_, { attrs, slots }) {
      return () => h(tag, { ...defaults, ...attrs }, slots.default?.())
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
export const UiForm = element('form', { novalidate: '' })
export const UiList = element('ul')
export const UiListItem = element('li')
export const UiAnchor = element('a')
export const UiButton = element('button', { type: 'button' })
export const UiTime = element('time')
export const UiImage = element('img')
