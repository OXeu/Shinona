import { toast } from '@yusufceng1z/ding'
import { readonly, shallowRef } from 'vue'

const host = shallowRef<HTMLElement>()
const dialogs: HTMLElement[] = []
export const notificationHost = readonly(host)

export function useDialogNotificationHost(element: HTMLElement) {
  dialogs.push(element)
  host.value = element
  return () => {
    const index = dialogs.lastIndexOf(element)
    if (index < 0) return
    dialogs.splice(index, 1)
    host.value = dialogs.at(-1)
  }
}

export const notificationView = {
  state: toast.getState,
  subscribe: toast.subscribe,
  pause: toast.pause,
  resume: toast.resume,
  dismiss: toast.dismiss,
}

export const notify = {
  success(message: string) {
    toast.success(message)
  },
  error(message: string) {
    toast.error(message)
  },
}
