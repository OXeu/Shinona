export class HttpError extends Error {
  constructor(readonly status: number, message: string, readonly code?: string) {
    super(message)
  }
}

export async function request(path: string, body?: unknown, signal?: AbortSignal, method = body === undefined ? 'GET' : 'POST'): Promise<Response> {
  const response = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  })
  if (!response.ok) {
    const code = response.headers.get('X-Shinona-Error-Code') ?? undefined
    let message = (await response.text()).trim()
    if (response.headers.get('Content-Type')?.startsWith('application/json')) {
      try {
        message = (JSON.parse(message) as { message?: string }).message ?? message
      } catch {
        // The proxy can return a non-JSON error response.
      }
    }
    throw new HttpError(response.status, message || 'Request failed.', code)
  }

  return response
}

export async function requestJSON<T>(path: string, body?: unknown, signal?: AbortSignal, method?: string): Promise<T> {
  const response = await request(path, body, signal, method)
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

export function apiJSON<T>(path: string, method: string, signal?: AbortSignal, body?: unknown): Promise<T> {
  return requestJSON<T>('/api/v1' + path, body, signal, method)
}
