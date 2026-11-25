import type { Turn, TurnPart } from './sessionApi'

export type ToolCategory = 'command' | 'read' | 'change' | 'tool' | 'thinking'
type Part = TurnPart & { index: number }
type Chunk = { kind: 'text' | 'ui'; part: Part } | { kind: 'process'; index: number; parts: Part[] }

export const activityIcons: Record<ToolCategory, string> = {
  command: 'terminal-line',
  read: 'file-text-line',
  change: 'file-edit-line',
  tool: 'tools-line',
  thinking: 'brain-line',
}

export function toolCategory(name: string): ToolCategory {
  switch (name) {
    case 'bash': return 'command'
    case 'read': return 'read'
    case 'write': return 'change'
    default: return 'tool'
  }
}

export function messageChunks({ parts, status, output }: Pick<Turn, 'parts' | 'status' | 'output'>): Chunk[] {
  let foldBefore = -1
  if (status === 'SUCCEEDED') {
    foldBefore = output.trim()
      ? parts.length
      : parts.findLastIndex((part) => part.kind === 'text' && !!part.text.trim())
  }
  const chunks: Chunk[] = []
  for (const [index, part] of parts.entries()) {
    if (part.kind === 'failure') continue
    const item = { ...part, index }
    if (item.kind === 'ui') {
      chunks.push({ kind: 'ui', part: item })
      continue
    }
    if (item.kind === 'text' && index >= foldBefore) {
      chunks.push({ kind: 'text', part: item })
      continue
    }
    const previous = chunks.at(-1)
    if (previous?.kind === 'process') previous.parts.push(item)
    else chunks.push({ kind: 'process', index, parts: [item] })
  }
  return chunks
}

export function hasSettled(turn: Turn) {
  return turn.status !== 'RUNNING' && turn.status !== 'QUEUED'
}

export function activityCategories(parts: TurnPart[], active: boolean): ToolCategory[] {
  const tools = parts.filter((part) => part.kind === 'tool')
  if (active) {
    const current = tools.findLast((part) => part.status === 'QUEUED' || part.status === 'RUNNING')
    return [current ? toolCategory(current.tool) : 'thinking']
  }
  if (!tools.length) return ['thinking']
  const counts = new Map<ToolCategory, number>()
  for (const { tool } of tools) {
    const category = toolCategory(tool)
    counts.set(category, (counts.get(category) ?? 0) + 1)
  }
  return [...counts].sort((a, b) => b[1] - a[1]).map(([category]) => category)
}
