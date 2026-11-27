import { parsePatchFiles } from '@pierre/diffs'
import type { FileDiffMetadata } from '@pierre/diffs'

export interface WorkspaceInspection {
  title: string
  path: string
  diff: boolean
  status?: string
  revision?: string
}

const gitPatch = /^diff --git /mu
const combinedPatch = /^diff --(?:cc|combined) /mu

/**
 * Parses a workspace preview that the server rendered as a Git patch.
 * Combined merge diffs have no supported representation, so they fall back to
 * the plain text viewer alongside non-patch previews.
 */
export function workspacePatchFiles(text: string): FileDiffMetadata[] {
  if (combinedPatch.test(text) || !gitPatch.test(text)) return []
  try {
    return parsePatchFiles(text).flatMap(({ files }) => files)
  } catch {
    return []
  }
}
