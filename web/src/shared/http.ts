export class HttpError extends Error {
  constructor(readonly status: number, message: string, readonly code?: string) {
    super(message)
  }
}

export async function request(path: string, body?: unknown, signal?: AbortSignal): Promise<Response> {
  const response = await fetch(path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  })
  if (!response.ok) {
    const text = await response.text()
    throw new HttpError(response.status, text.trim() || 'Request failed.', response.headers.get('X-Shinona-Error-Code') ?? undefined)
  }

  return response
}

export async function requestJSON<T>(path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const response = await request(path, body, signal)
  if (response.status === 204) return undefined as T
  return await response.json() as T
}
