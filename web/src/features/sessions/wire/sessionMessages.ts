import type { ComposerRunOptions, Policy, RunStatus, SandboxSettings, SessionSummary, Turn, TurnPart } from '../sessionApi'
import type { RunOptions, SandboxSettings as SettingsMessage, SessionSummary as SessionMessage, Turn as TurnMessage, TurnPart as PartMessage } from './session_transfer_pb'

export function decodeRunOptions(options: RunOptions | undefined): ComposerRunOptions | null {
  if (!options) return null
  const { model, permission, thinking, mcpServers, arrowJs } = options
  return { model, permission, thinking, mcpServers, arrowJs }
}

export function decodeSessionSummary(message: SessionMessage): SessionSummary {
  const { id, projectId, title, defaultTitle, turnCount, updatedAt, pinned, active, unread } = message
  return { id, projectId: projectId ?? null, title, defaultTitle, turnCount, updatedAt, pinned, active, unread }
}

export function decodeTurn(message: TurnMessage): Turn {
  const { id, sequence, prompt, status, policy, output, createdAt, startedAt, updatedAt, model, attachments, contextTokens, contextWindow } = message
  return {
    id, sequence, prompt, status: status as RunStatus, policy: (policy ?? null) as Policy | null,
    output, createdAt, startedAt: startedAt ?? null, updatedAt, model, attachments, contextTokens, contextWindow,
    ...(message.liveContext ? { liveContext: message.liveContext } : {}),
    ...(message.tokenUsage ? { tokenUsage: message.tokenUsage } : {}),
    steering: message.steering.map(({ text, delivered }) => ({ text, delivered })),
    parts: message.parts.map(decodeTurnPart),
  }
}

export function decodeTurnPart({ kind, text, tool, input, output, status, estimatedTokens }: PartMessage): TurnPart {
  const part = { kind: kind as TurnPart['kind'], text, tool, input, output, status: status as RunStatus | '' }
  return estimatedTokens > 0 ? { ...part, estimatedTokens } : part
}

export function decodeSandboxSettings(settings: SettingsMessage): SandboxSettings {
  return { selected: (settings.selected ?? null) as Policy | null, available: settings.available as Policy[] }
}
