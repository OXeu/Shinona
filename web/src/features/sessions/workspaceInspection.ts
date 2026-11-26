export interface WorkspaceInspection {
  title: string
  path: string
  diff: boolean
  status?: string
  revision?: string
}

export function diffLines(text: string) {
  const lines = text.split('\n')
  if (lines.at(-1) === '') lines.pop()
  let inHunk = false
  return lines.map((line) => {
    let kind = ''
    if (line.startsWith('diff --git ')) inHunk = false
    if (line.startsWith('@@')) {
      inHunk = true
      kind = 'hunk'
    } else if (inHunk && line.startsWith('+')) {
      kind = 'addition'
    } else if (inHunk && line.startsWith('-')) {
      kind = 'deletion'
    }
    return { line, kind }
  })
}
