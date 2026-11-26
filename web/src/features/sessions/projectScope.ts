export type ProjectTarget =
  | { readonly kind: 'default' }
  | { readonly kind: 'project'; readonly id: string }

export type ProjectScope = ProjectTarget | { readonly kind: 'all' }

export const allProjects = { kind: 'all' } as const
export const defaultProject = { kind: 'default' } as const

export function projectScopeKey(scope: ProjectScope): string {
  return scope.kind === 'project' ? 'project:' + scope.id : scope.kind
}

export function projectTargetId(project: ProjectTarget): string | null {
  return project.kind === 'project' ? project.id : null
}

export function includesProject(scope: ProjectScope, id: string | null): boolean {
  return scope.kind === 'all' || projectTargetId(scope) === id
}
