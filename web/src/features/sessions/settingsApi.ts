import { graphql } from '../../shared/graphql'
import { mcpSettingsFields } from './mcpSettingsApi'
import type { McpSettings } from './mcpSettingsApi'

export interface SettingField {
  key: string
  value: string | null
  configured: boolean
  overridden: boolean
}

const settingFields = 'key value configured overridden'

export const settingsApi = {
  fetchModels: (baseUrl: string | undefined, apiKey: string | undefined, signal: AbortSignal) =>
    graphql<{ fetchModels: { id: string }[] }>(`
      mutation FetchModels($baseUrl: String, $apiKey: String) {
        fetchModels(baseUrl: $baseUrl, apiKey: $apiKey) { id }
      }
    `, { baseUrl, apiKey }, signal),
  load: (signal: AbortSignal) =>
    graphql<{ applicationSettings: SettingField[]; modelSettings: SettingField[]; mcpSettings: McpSettings }>(`
      query Settings {
        applicationSettings { ${settingFields} }
        modelSettings { ${settingFields} }
        mcpSettings { ${mcpSettingsFields} }
      }
    `, {}, signal),
  save: (changes: { key: string; value: string | null }[], signal: AbortSignal) =>
    graphql<{ updateApplicationSettings: SettingField[] }>(`
      mutation UpdateApplicationSettings($changes: [SettingChange!]!) {
        updateApplicationSettings(changes: $changes) { ${settingFields} }
      }
    `, { changes }, signal),
  saveModel: (changes: { key: string; value: string | null }[], signal: AbortSignal) =>
    graphql<{ updateModelSettings: SettingField[] }>(`
      mutation UpdateModelSettings($changes: [SettingChange!]!) {
        updateModelSettings(changes: $changes) { ${settingFields} }
      }
    `, { changes }, signal),
}
