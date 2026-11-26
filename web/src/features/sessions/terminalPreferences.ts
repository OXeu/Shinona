const activeTabs = new Map<string, string>()

function storageKey(owner: string, session: string) {
  return 'shinona.terminal-tab.' + owner + '.' + session
}

export function activeTerminalTab(owner: string | undefined, session: string) {
  if (!owner) return null
  const key = storageKey(owner, session)
  try {
    return window.localStorage.getItem(key)
  } catch {
    return activeTabs.get(key) ?? null
  }
}

export function rememberTerminalTab(owner: string | undefined, session: string, tab: string | null) {
  if (!owner) return
  const key = storageKey(owner, session)
  try {
    if (tab) window.localStorage.setItem(key, tab)
    else window.localStorage.removeItem(key)
    activeTabs.delete(key)
  } catch {
    if (tab) activeTabs.set(key, tab)
    else activeTabs.delete(key)
  }
}
