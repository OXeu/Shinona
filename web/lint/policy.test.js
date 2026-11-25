import assert from 'node:assert/strict'
import test from 'node:test'

import { Linter } from 'eslint'
import vueParser from 'vue-eslint-parser'

import { componentTemplates, nativeControls, notificationGateway, toastOnly } from './policy.js'

const linter = new Linter()
const localRules = {
  shinona: { rules: { 'toast-only': toastOnly, 'notification-gateway': notificationGateway, 'component-templates': componentTemplates, 'native-controls': nativeControls } },
}

test('template policies reject conditional directives and inline live feedback', () => {
  const source = `<template>
    <p v-if="failed">Failed</p>
    <p v-else-if="loading">Loading</p>
    <p v-else>Done</p>
    <p v-show="retrying">Retrying</p>
    <p role="alert">Failed</p>
    <p :role="'status'">Saved</p>
    <p :aria-live="politeness">Ready</p>
    <output>Saved</output>
  </template>`

  const errors = linter.verify(source, [{
    files: ['**/*.vue'],
    languageOptions: { parser: vueParser },
    plugins: localRules,
    rules: { 'shinona/toast-only': 'error' },
  }], { filename: 'Example.vue' })

  assert.deepEqual(errors.map((error) => error.messageId), [
    'conditional', 'conditional', 'conditional', 'conditional',
    'inline', 'inline', 'inline', 'inline',
  ])
})

test('run state can be displayed as ordinary data', () => {
  const errors = linter.verify('<template><span>{{ run.status }}</span></template>', [{
    files: ['**/*.vue'],
    languageOptions: { parser: vueParser },
    plugins: localRules,
    rules: { 'shinona/toast-only': 'error' },
  }], { filename: 'Example.vue' })

  assert.deepEqual(errors, [])
})

test('Ding imports and re-exports go through the notification facade', () => {
  const errors = linter.verify(`
    import { toast } from '@yusufceng1z/ding-vue'
    export * from '@yusufceng1z/ding'
    const other = import('@yusufceng1z/ding-react')
    import { notify } from './notifications.ts'
  `, [{ plugins: localRules, rules: { 'shinona/notification-gateway': 'error' } }])
  assert.deepEqual(errors.map((error) => error.messageId), ['gateway', 'gateway', 'gateway'])
})

test('feature templates reject native elements and dynamic raw tags', () => {
  const config = [{
    files: ['**/*.vue'],
    languageOptions: { parser: vueParser },
    plugins: localRules,
    rules: { 'shinona/component-templates': 'error' },
  }]
  const source = `<template>
    <div><h1>Title</h1><h2>Subheading</h2><button>Save</button></div>
    <table><tr><td>Value</td></tr></table>
    <tl/><input/><textarea/><svg/><component :is="'button'"/>
  </template>`
  const invalid = linter.verify(source, config, { filename: 'Example.vue' })
  assert.deepEqual(invalid.map((error) => error.messageId), Array(12).fill('raw'))

  const valid = linter.verify('<template><UiBox><UiButton>Save</UiButton><slot/></UiBox></template>', config, { filename: 'Example.vue' })
  assert.deepEqual(valid, [])
})

test('native choice and dialog controls have explicit replacements', () => {
  const config = {
    files: ['**/*.vue'],
    languageOptions: { parser: vueParser },
    plugins: localRules,
  }
  const rules = { 'shinona/native-controls': 'error' }
  const source = `<template>
    <select><optgroup><option>One</option></optgroup></select>
    <dialog/><alert/>
    <UiButton @click="window['alert']('x')"/>
    <UiButton @click="confirm('x')"/>
    <UiButton @click="self.prompt('x')"/>
  </template>`
  const errors = linter.verify(source, [{ ...config, rules }], { filename: 'Example.vue' })
  assert.deepEqual(errors.map((error) => error.messageId), [
    'element', 'element', 'element', 'element', 'element', 'browserDialog', 'browserDialog', 'browserDialog',
  ])

  const shared = linter.verify('<template><dialog><select/></dialog></template>', [
    { ...config, rules: { 'shinona/native-controls': ['error', { allowDialog: true }] } },
  ], { filename: 'UiDialog.vue' })
  assert.deepEqual(shared.map((error) => error.message), ['Replace <select> with UiChoiceDropdown.'])
})
