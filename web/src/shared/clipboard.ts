export async function copyText(text: string) {
  if (navigator.clipboard) return navigator.clipboard.writeText(text)

  // Private HTTP previews cannot use the secure-context Clipboard API.
  const field = document.createElement('textarea')
  field.value = text
  field.readOnly = true
  field.tabIndex = -1
  field.style.cssText = 'position: fixed; inset: 0 auto auto 0; opacity: 0; pointer-events: none'
  const previousFocus = document.activeElement
  document.body.append(field)
  try {
    field.select()
    if (!document.execCommand('copy')) throw new Error('Clipboard copy failed')
  } finally {
    field.remove()
    if (previousFocus instanceof HTMLElement) previousFocus.focus({ preventScroll: true })
  }
}
