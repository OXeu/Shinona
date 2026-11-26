import js from '@eslint/js'
import vue from 'eslint-plugin-vue'
import ts from 'typescript-eslint'
import vueParser from 'vue-eslint-parser'

import { componentTemplates, nativeControls, notificationGateway, toastOnly } from './lint/policy.js'
import { noOutlineOffset } from './lint/style-policy.js'

const local = {
  rules: {
    'toast-only': toastOnly,
    'notification-gateway': notificationGateway,
    'component-templates': componentTemplates,
    'native-controls': nativeControls,
    'no-outline-offset': noOutlineOffset,
  },
}

export default [
  { ignores: ['dist/**', 'src/features/sessions/wire/*_pb.ts'] },
  { linterOptions: { noInlineConfig: true, reportUnusedDisableDirectives: 'error' } },
  js.configs.recommended,
  { rules: { 'no-alert': 'error' } },
  ...ts.configs.recommended,
  ...vue.configs['flat/recommended'],
  {
    files: ['**/*.{js,ts,vue}'],
    plugins: { shinona: local },
    rules: { 'shinona/no-outline-offset': 'error' },
  },
  {
    files: ['src/**/*.{ts,vue}'],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
        extraFileExtensions: ['.vue'],
      },
    },
    rules: {
      '@typescript-eslint/consistent-type-imports': 'error',
      '@typescript-eslint/no-floating-promises': 'error',
      '@typescript-eslint/no-explicit-any': 'error',
      'no-console': 'error',
      'shinona/notification-gateway': 'error',
    },
  },
  {
    files: ['src/**/*.vue'],
    languageOptions: { parser: vueParser, parserOptions: { parser: ts.parser } },
    rules: {
      'shinona/toast-only': 'error',
      'shinona/native-controls': 'error',
      'vue/no-undef-components': 'error',
      'vue/no-v-html': 'error',
      'vue/html-button-has-type': 'error',
      'vue/max-attributes-per-line': ['warn', { singleline: 3, multiline: 1 }],
      'vue/singleline-html-element-content-newline': 'off',
      'no-undef': 'off',
    },
  },
  {
    files: ['src/shared/ui/UiDialog.vue'],
    rules: { 'shinona/native-controls': ['error', { allowDialog: true }] },
  },
  {
    files: ['src/**/*.vue'],
    ignores: ['src/shared/ui/**'],
    rules: { 'shinona/component-templates': 'error' },
  },
  {
    files: ['src/**/*.{ts,vue}'],
    ignores: ['src/shared/ui/**'],
    rules: {
      'no-restricted-imports': ['error', {
        paths: [{
          name: 'vue',
          importNames: ['h', 'createVNode', 'createElementVNode', 'createElementBlock'],
          message: 'Compose UI components. Native rendering belongs in src/shared/ui.',
        }],
      }],
    },
  },
  {
    files: ['src/**/*.{ts,vue}'],
    ignores: ['src/shared/http.ts'],
    rules: {
      'no-restricted-globals': ['error', { name: 'fetch', message: 'Use src/shared/http.ts.' }],
      'no-restricted-properties': [
        'error',
        { object: 'window', property: 'fetch', message: 'Use src/shared/http.ts.' },
        { object: 'globalThis', property: 'fetch', message: 'Use src/shared/http.ts.' },
      ],
    },
  },
  {
    files: ['src/shared/notifications.ts'],
    rules: { 'shinona/notification-gateway': 'off' },
  },
  {
    files: ['src/shared/ui/UiToaster.vue'],
    rules: { 'shinona/toast-only': 'off' },
  },
]
