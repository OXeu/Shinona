import { graphql } from '../../shared/graphql'
import { requestJSON } from '../../shared/http'

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

export const mcpSettingsFields = 'servers { name url hasToken oauth connected } overridden'

export const mcpSettingsApi = {
  connect: (name: string, signal: AbortSignal) => requestJSON<{ url: string }>('/auth/mcp/start', { name }, signal),
  disconnect: (name: string, signal: AbortSignal) => requestJSON<void>('/auth/mcp/disconnect', { name }, signal),
  save: (input: McpServerInput, signal: AbortSignal) =>
    graphql<{ saveMcpServer: McpSettings }>(`
      mutation SaveMcpServer($input: McpServerInput!) {
        saveMcpServer(input: $input) { ${mcpSettingsFields} }
      }
    `, { input }, signal),
  remove: (name: string, signal: AbortSignal) =>
    graphql<{ removeMcpServer: McpSettings }>(`
      mutation RemoveMcpServer($name: String!) {
        removeMcpServer(name: $name) { ${mcpSettingsFields} }
      }
    `, { name }, signal),
  reset: (signal: AbortSignal) =>
    graphql<{ resetMcpSettings: McpSettings }>(`
      mutation ResetMcpSettings {
        resetMcpSettings { ${mcpSettingsFields} }
      }
    `, {}, signal),
}
