import { apiJSON, requestJSON } from '../../shared/http'

export interface McpServer {
  name: string
  url: string
  hasToken: boolean
  oauth: boolean
  connected: boolean
}

export interface McpSettings {
  servers: McpServer[]
  overridden: boolean
}

interface McpServerInput {
  previousName: string | null
  name: string
  url: string
  token: string | null
  oauth: boolean
}

export const mcpSettingsApi = {
  connect: (name: string, signal: AbortSignal) => requestJSON<{ url: string }>('/auth/mcp/start', { name }, signal),
  disconnect: (name: string, signal: AbortSignal) => requestJSON<void>('/auth/mcp/disconnect', { name }, signal),
  save: (input: McpServerInput, signal: AbortSignal) =>
    apiJSON<McpSettings>('/settings/mcp/servers', 'PUT', signal, input),
  remove: (name: string, signal: AbortSignal) =>
    apiJSON<McpSettings>('/settings/mcp/servers?' + new URLSearchParams({ name }), 'DELETE', signal),
  reset: (signal: AbortSignal) =>
    apiJSON<McpSettings>('/settings/mcp', 'DELETE', signal),
}
