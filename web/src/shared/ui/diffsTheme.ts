import { createCSSVariablesTheme, registerCustomTheme } from '@pierre/diffs'
import type { DiffsTheme } from '@pierre/diffs'

// Register the shadow DOM host used by the vanilla Diffs renderers. The
// package ships its base stylesheet through this custom element definition.
import '@pierre/diffs/web-components'

const palette = {
  background: 'var(--surface)',
  foreground: 'var(--foreground)',
  'token-comment': 'var(--muted)',
  'token-constant': 'var(--warning)',
  'token-deleted': 'var(--danger)',
  'token-function': 'var(--primary)',
  'token-inserted': 'var(--success)',
  'token-keyword': 'var(--link)',
  'token-link': 'var(--link)',
  'token-parameter': 'var(--foreground-soft)',
  'token-punctuation': 'var(--muted)',
  'token-string': 'var(--success)',
  'token-string-expression': 'var(--success)',
  'ansi-black': 'var(--foreground)',
  'ansi-red': 'var(--danger)',
  'ansi-green': 'var(--success)',
  'ansi-yellow': 'var(--warning)',
  'ansi-blue': 'var(--link)',
  'ansi-magenta': 'var(--primary)',
  'ansi-cyan': 'var(--link)',
  'ansi-white': 'var(--foreground)',
  'ansi-bright-black': 'var(--muted)',
  'ansi-bright-red': 'var(--danger)',
  'ansi-bright-green': 'var(--success)',
  'ansi-bright-yellow': 'var(--warning)',
  'ansi-bright-blue': 'var(--link)',
  'ansi-bright-magenta': 'var(--primary)',
  'ansi-bright-cyan': 'var(--link)',
  'ansi-bright-white': 'var(--foreground)',
} as const

function shinonaTheme(type: 'light' | 'dark'): DiffsTheme {
  const theme = createCSSVariablesTheme({
    name: 'shinona-' + type,
    variableDefaults: palette,
    fontStyle: false,
  })
  theme.type = type
  if (theme.zed) theme.zed.appearance = type
  return theme
}

// Register light and dark variants so Diffs follows the preferred color scheme
// instead of forcing the single theme type that CSS-variable themes default to.
registerCustomTheme('shinona-dark', () => Promise.resolve(shinonaTheme('dark')), 'diffs')
registerCustomTheme('shinona-light', () => Promise.resolve(shinonaTheme('light')), 'diffs')

const unsafeCSS = `
:host {
  --diffs-font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  --diffs-font-size: 12px;
  --diffs-line-height: 18px;
  --diffs-tab-size: 2;
  --diffs-gap-fallback: 0px;
  --diffs-light-addition-color: var(--success);
  --diffs-dark-addition-color: var(--success);
  --diffs-light-deletion-color: var(--danger);
  --diffs-dark-deletion-color: var(--danger);
  --diffs-light-modified-color: var(--link);
  --diffs-dark-modified-color: var(--link);
}
`

export const codeViewOptions = {
  theme: { dark: 'shinona-dark', light: 'shinona-light' },
  preferredHighlighter: 'highlights',
  unsafeCSS,
} as const
