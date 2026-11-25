function reply(socket: WebSocket, payload: Uint8Array<ArrayBuffer>, signal: AbortSignal): Promise<ArrayBuffer> {
  return new Promise((resolve, reject) => {
    signal.throwIfAborted()
    const events = new AbortController()
    const options = { signal: events.signal }
    const finish = () => {
      events.abort()
      signal.removeEventListener('abort', abort)
    }
    const fail = () => {
      finish()
      reject(new Error('The session list connection closed.'))
    }
    const send = () => {
      try {
        socket.send(payload)
      } catch {
        fail()
      }
    }
    const abort = () => {
      finish()
      reject(signal.reason)
    }
    signal.addEventListener('abort', abort, { once: true })
    socket.addEventListener('error', fail, options)
    socket.addEventListener('close', fail, options)
    socket.addEventListener('message', ({ data }) => {
      if (!(data instanceof ArrayBuffer)) {
        fail()
        return
      }
      finish()
      resolve(data)
    }, options)
    if (socket.readyState === WebSocket.OPEN) send()
    else socket.addEventListener('open', send, { ...options, once: true })
  })
}

// One poll at a time keeps responses paired with the current project filter.
export class RecentSocket {
  private socket?: WebSocket
  private pending = false

  async read(path: string, payload: Uint8Array<ArrayBuffer>, signal: AbortSignal): Promise<Uint8Array> {
    signal.throwIfAborted()
    if (this.pending) this.close()
    if (this.socket?.readyState !== WebSocket.OPEN) {
      this.close()
      const url = new URL(path, location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      this.socket = new WebSocket(url)
      this.socket.binaryType = 'arraybuffer'
    }

    const socket = this.socket
    this.pending = true
    try {
      const response = await reply(socket, payload, AbortSignal.any([signal, AbortSignal.timeout(5_000)]))
      return new Uint8Array(response)
    } catch (error) {
      if (this.socket === socket) this.close()
      throw error
    } finally {
      if (this.socket === socket) this.pending = false
    }
  }

  close() {
    this.socket?.close()
    this.socket = undefined
    this.pending = false
  }
}
