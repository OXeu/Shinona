import { requestJSON } from '../../shared/http'

export interface TerminalTab {
  id: string
  number: number
}

const path = '/api/terminal/v1'

export const terminalApi = {
  list: (session: string, signal: AbortSignal) =>
    requestJSON<TerminalTab[]>(path + '?' + new URLSearchParams({ session }), undefined, signal),
  create: (session: string, signal: AbortSignal) =>
    requestJSON<TerminalTab>(path + '/create', { session }, signal),
  close: (session: string, terminal: string, signal: AbortSignal) =>
    requestJSON<void>(path + '/close', { session, terminal }, signal),
  connect(session: string, terminal: string) {
    const url = new URL(path + '/connect', location.href)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    url.search = new URLSearchParams({ session, terminal }).toString()
    return new WebSocket(url)
  },
}
