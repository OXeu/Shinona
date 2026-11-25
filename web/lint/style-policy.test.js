import assert from 'node:assert/strict'
import test from 'node:test'

import { Linter } from 'eslint'
import vueParser from 'vue-eslint-parser'

import { noOutlineOffset, outlineOffsetDeclarations } from './style-policy.js'

const linter = new Linter()
const policy = {
  plugins: { shinona: { rules: { 'no-outline-offset': noOutlineOffset } } },
  rules: { 'shinona/no-outline-offset': 'error' },
}

test('the stylesheet check rejects outline offsets and allows outlines, comments and selectors', () => {
  const css = `/* outline-offset: 2px */
.outline-offset:focus-visible {
  outline: 2px solid;
  OUTLINE-OFFSET /* folded name */ : 2px;
  --outline-offset: 3px;
}`
  const positions = outlineOffsetDeclarations(css)
  assert.deepEqual(positions.map(({ line, column }) => ({ line, column })), [{ line: 4, column: 3 }])
})

test('the Vue rule rejects offsets in component style blocks and inline bindings', () => {
  const source = `<template>
    <UiBox style="outline-offset: 2px"/>
    <UiBox :style="{ outlineOffset: '2px' }"/>
    <UiBox :style="'outline-offset: 2px'"/>
    <UiBox :style="{ outline: '2px solid' }"/>
  </template>
  <style scoped>
  button { outline-offset: -2px }
  </style>`
  const errors = linter.verify(source, [{
    ...policy,
    files: ['**/*.vue'],
    languageOptions: { parser: vueParser },
  }], { filename: 'Example.vue' })
  assert.deepEqual(errors.map(({ line, messageId }) => [line, messageId]), [
    [2, 'forbidden'], [3, 'forbidden'], [4, 'forbidden'], [8, 'forbidden'],
  ])
})

const less = `@offset: 2px;
// outline-offset: 2px;
.example {
  &:focus { outline: 2px solid; outline-offset: @offset; }
}`

test('the stylesheet check detects outline offsets in Less rules', () => {
  const positions = outlineOffsetDeclarations(less, 'example.less')
  assert.deepEqual(positions.map(({ line, column }) => ({ line, column })), [{ line: 4, column: 33 }])
})

test('the Vue rule parses Less style blocks and reports the original line', () => {
  const source = `<template><UiBox /></template>
<style lang="less" scoped>
${less}
</style>`
  const errors = linter.verify(source, [{
    ...policy,
    files: ['**/*.vue'],
    languageOptions: { parser: vueParser },
  }], { filename: 'Example.vue' })
  assert.deepEqual(errors.map(({ line, messageId }) => [line, messageId]), [[6, 'forbidden']])
})

test('the script rule rejects style objects, direct assignments and setProperty', () => {
  const source = `
    const style = { 'outline-offset': '2px' }
    element.style.outlineOffset = '2px'
    element.style.setProperty('outline-offset', '2px')
    element.style.outline = '2px solid'
  `
  const errors = linter.verify(source, [policy])
  assert.deepEqual(errors.map(({ line, messageId }) => [line, messageId]), [
    [2, 'forbidden'], [3, 'forbidden'], [4, 'forbidden'],
  ])
})
