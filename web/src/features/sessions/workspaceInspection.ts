export interface WorkspaceInspection {
  title: string
  path: string
  diff: boolean
  status?: string
  revision?: string
}

const fileHeader = /^diff --(?:git|cc|combined) /u

export function diffLines(text: string) {
  if (!fileHeader.test(text)) return []
  const lines = text.split('\n')
  if (lines.at(-1) === '') lines.pop()
  let parentCount = 0
  return lines.map((line) => {
    if (fileHeader.test(line)) parentCount = 0
    const hunk = /^@{2,}(?= )/u.exec(line)
    if (hunk) {
      parentCount = hunk[0].length - 1
      return { line, kind: 'hunk' }
    }
    const markers = line.slice(0, parentCount)
    if (markers.includes('+')) return { line, kind: 'addition' }
    if (markers.includes('-')) return { line, kind: 'deletion' }
    return { line, kind: '' }
  })
}
