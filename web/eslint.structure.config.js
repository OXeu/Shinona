import ts from 'typescript-eslint'
import vueParser from 'vue-eslint-parser'

export default [
  { ignores: ['src/features/sessions/wire/*_pb.ts'] },
  { linterOptions: { noInlineConfig: true } },
  {
    files: ['src/**/*.{js,ts,vue}'],
    languageOptions: { parser: ts.parser },
    rules: {
      complexity: ['warn', 20],
      'max-len': ['warn', { code: 140, ignoreUrls: true }],
      'max-lines': ['warn', 400],
      'max-statements-per-line': ['warn', { max: 1 }],
    },
  },
  {
    files: ['src/**/*.vue'],
    languageOptions: { parser: vueParser, parserOptions: { parser: ts.parser } },
  },
]
