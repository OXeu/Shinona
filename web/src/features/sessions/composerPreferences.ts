import { readonly, ref } from 'vue'
import type { ComposerDefaults, ComposerOptions } from './sessionApi'

export function resolveComposerDefaults(catalog: ComposerOptions): ComposerDefaults {
  const saved = catalog.defaults
  return {
    model: saved?.model && (catalog.modelsLoading || catalog.models.some(({ id }) => id === saved.model))
      ? saved.model : catalog.defaultModel,
    permission: saved?.permission ?? 'WORKSPACE',
    thinking: saved?.thinking ?? 'off',
  }
}

export interface ComposerSelection {
  model?: string | null
  permission?: string | null
  thinking?: string | null
  mcpServers?: string[]
  jsonRender?: boolean
}

function sessionKey(owner: string, session: string) {
  return `shinona.composer.${owner}.${session}`
}

export function readComposerSelection(owner: string, session: string): ComposerSelection {
  try {
    const saved: unknown = JSON.parse(window.localStorage.getItem(sessionKey(owner, session)) ?? 'null')
    if (!saved || typeof saved !== 'object') return {}
    const result: ComposerSelection = {}
    for (const key of ['model', 'permission', 'thinking'] as const) {
      if (!(key in saved)) continue
      const value = (saved as Record<string, unknown>)[key]
      if (typeof value === 'string' || value === null) result[key] = value
    }
    if ('mcpServers' in saved && Array.isArray(saved.mcpServers)) {
      result.mcpServers = saved.mcpServers.filter((name): name is string => typeof name === 'string')
    }
    if ('jsonRender' in saved && typeof saved.jsonRender === 'boolean') result.jsonRender = saved.jsonRender
    return result
  } catch {
    return {}
  }
}

export function writeComposerSelection(owner: string, session: string, selection?: ComposerSelection) {
  try {
    const key = sessionKey(owner, session)
    if (selection) window.localStorage.setItem(key, JSON.stringify(selection))
    else window.localStorage.removeItem(key)
  } catch {
    // The store keeps the selection for the page if browser storage is disabled.
  }
}

export type RunningAction = 'queue' | 'steer'
const key = 'shinona.runningAction'

function readPreference(): RunningAction {
  try {
    if (window.localStorage.getItem(key) === 'steer') return 'steer'
  } catch {
    // A browser can disable storage.
  }
  return 'queue'
}

const selected = ref<RunningAction>(readPreference())
export const runningAction = readonly(selected)

export function setRunningAction(value: string) {
  if (value !== 'queue' && value !== 'steer') return
  selected.value = value
  try {
    window.localStorage.setItem(key, value)
  } catch {
    // Use the selection for this page even without storage.
  }
}
