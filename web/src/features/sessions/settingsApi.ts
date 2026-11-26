import { graphql } from '../../shared/graphql'
import { mcpSettingsFields } from './mcpSettingsApi'
import type { McpSettings } from './mcpSettingsApi'

export interface SettingField {
  key: string
  value: string | null
  configured: boolean
  overridden: boolean
}

export interface SSHServer {
  id: string
  name: string
  address: string
  user: string
  hostFingerprint: string
  template: string
}

export interface SSHServerInput extends SSHServer {
  privateKey: string
}

const settingFields = 'key value configured overridden'
const sshServerFields = 'id name address user hostFingerprint template'

export const settingsApi = {
  fetchModels: (baseUrl: string | undefined, apiKey: string | undefined, signal: AbortSignal) =>
    graphql<{ fetchModels: { id: string }[] }>(`
      mutation FetchModels($baseUrl: String, $apiKey: String) {
        fetchModels(baseUrl: $baseUrl, apiKey: $apiKey) { id }
      }
    `, { baseUrl, apiKey }, signal),
  load: (signal: AbortSignal) =>
    graphql<{ applicationSettings: SettingField[]; modelSettings: SettingField[]; mcpSettings: McpSettings; sshServers: SSHServer[] }>(`
      query Settings {
        applicationSettings { ${settingFields} }
        modelSettings { ${settingFields} }
        mcpSettings { ${mcpSettingsFields} }
        sshServers { ${sshServerFields} }
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
  saveSSHServer: (input: SSHServerInput, signal: AbortSignal) =>
    graphql<{ saveSshServer: SSHServer }>(`
      mutation SaveSSHServer($input: SSHServerInput!) {
        saveSshServer(input: $input) { ${sshServerFields} }
      }
    `, { input }, signal),
  testSSHServer: (input: SSHServerInput, signal: AbortSignal) =>
    graphql<{ testSshServer: boolean }>(`
      mutation TestSSHServer($input: SSHServerInput!) { testSshServer(input: $input) }
    `, { input }, signal),
  removeSSHServer: (id: string, signal: AbortSignal) =>
    graphql<{ removeSshServer: boolean }>(`
      mutation RemoveSSHServer($id: ID!) { removeSshServer(id: $id) }
    `, { id }, signal),
}
