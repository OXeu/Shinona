interface TerminalView {
  open: boolean
  active: string | null
}

const defaultView: TerminalView = { open: false, active: null }
const views = new Map<string, TerminalView>()

function storageKey(owner: string, session: string) {
  return 'shinona.terminal.' + owner + '.' + session
}

export function terminalView(owner: string | undefined, session: string | undefined): TerminalView {
  if (!owner || !session) return defaultView
  const key = storageKey(owner, session)
  const cached = views.get(key)
  if (cached) return cached
  try {
    const saved: unknown = JSON.parse(window.localStorage.getItem(key) ?? 'null')
    if (saved && typeof saved === 'object' && 'open' in saved) {
      return { open: saved.open === true, active: 'active' in saved && typeof saved.active === 'string' ? saved.active : null }
    }
  } catch {
    // Keep the view in memory when browser storage is unavailable.
  }
  return defaultView
}

export function rememberTerminalView(owner: string | undefined, session: string | undefined, patch: Partial<TerminalView>) {
  if (!owner || !session) return
  const key = storageKey(owner, session)
  const view = { ...terminalView(owner, session), ...patch }
  views.set(key, view)
  try {
    window.localStorage.setItem(key, JSON.stringify(view))
    views.delete(key)
  } catch {
    // Keep the view in memory when browser storage is unavailable.
  }
}
