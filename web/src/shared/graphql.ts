import { requestJSON } from './http'

interface GraphQLResponse<T> {
  data?: T
  errors?: { message: string; extensions?: { code?: string } }[]
}

export class GraphQLError extends Error {
  constructor(message: string, readonly code?: string) {
    super(message)
  }
}

export async function graphql<T>(
  query: string,
  variables: Record<string, unknown> = {},
  signal?: AbortSignal,
): Promise<T> {
  const payload = await requestJSON<GraphQLResponse<T>>('/graphql', { query, variables }, signal)
  if (payload.errors?.length || payload.data === undefined) {
    const error = payload.errors?.[0]
    throw new GraphQLError(error?.message ?? 'The API returned an incomplete response.', error?.extensions?.code)
  }
  return payload.data
}
