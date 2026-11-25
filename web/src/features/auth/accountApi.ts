import { requestJSON } from '../../shared/http'
import type { AuthSession, User } from './authStore'

interface ProfileUpdate {
  name: string
  avatar?: string
  removeAvatar?: boolean
}

interface AccountPage {
  users: User[]
  total: number
  limit: number
}

type AccessChange = { admin: boolean } | { disabled: boolean }

export const accountApi = {
  updateProfile: (profile: ProfileUpdate) => requestJSON<AuthSession>('/auth/account/profile', profile),
  changePassword: (currentPassword: string, password: string) =>
    requestJSON<AuthSession>('/auth/account/password', { currentPassword, password }),
  sendEmailCode: (email: string, currentPassword: string) =>
    requestJSON<{ request: string }>('/auth/account/email/start', { email, currentPassword }),
  verifyEmail: (request: string, code: string) =>
    requestJSON<AuthSession>('/auth/account/email/verify', { request, code }),
  listUsers: (search: string, offset: number, signal: AbortSignal) => {
    const query = new URLSearchParams({ search, offset: String(offset) })
    return requestJSON<AccountPage>('/auth/admin/users?' + query.toString(), undefined, signal)
  },
  createUser: (email: string, password: string, name: string) =>
    requestJSON<User>('/auth/admin/users', { email, password, name }),
  updateAccess: (id: string, change: AccessChange) =>
    requestJSON<User>('/auth/admin/users/' + encodeURIComponent(id) + '/access', change),
}
