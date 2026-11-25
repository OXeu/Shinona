import { Toaster, toast } from '@yusufceng1z/ding-vue'

export { Toaster }

export const notify = {
  success(message: string) {
    toast.success(message)
  },
  error(message: string) {
    toast.error(message)
  },
}
