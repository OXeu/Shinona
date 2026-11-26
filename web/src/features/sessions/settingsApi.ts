import { apiJSON } from '../../shared/http'
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

export const settingsApi = {
  fetchModels: (baseUrl: string | undefined, apiKey: string | undefined, signal: AbortSignal) =>
    apiJSON<{ id: string; contextWindow: number }[]>('/settings/model/catalog', 'POST', signal, { baseUrl, apiKey }),
  load: (signal: AbortSignal) =>
    apiJSON<{ applicationSettings: SettingField[]; modelSettings: SettingField[]; mcpSettings: McpSettings; sshServers: SSHServer[] }>(
      '/settings', 'GET', signal),
  save: (changes: { key: string; value: string | null }[], signal: AbortSignal) =>
    apiJSON<SettingField[]>('/settings/application', 'PATCH', signal, { changes }),
  saveModel: (changes: { key: string; value: string | null }[], signal: AbortSignal) =>
    apiJSON<SettingField[]>('/settings/model', 'PATCH', signal, { changes }),
  saveSSHServer: (input: SSHServerInput, signal: AbortSignal) =>
    apiJSON<SSHServer>('/settings/ssh/servers', 'PUT', signal, input),
  testSSHServer: (input: SSHServerInput, signal: AbortSignal) =>
    apiJSON<void>('/settings/ssh/connection-test', 'POST', signal, input),
  removeSSHServer: (id: string, signal: AbortSignal) =>
    apiJSON<void>('/settings/ssh/servers/' + encodeURIComponent(id), 'DELETE', signal),
}
