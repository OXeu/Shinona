import { defineStore } from 'pinia'
import { ref } from 'vue'

import { requestJSON } from '../../shared/http'

export interface User {
  id: string
  email: string
  loginEmail: string
  name: string
  avatarUrl?: string
  hasAvatar: boolean
  hasPassword: boolean
  admin: boolean
  disabled: boolean
}

export interface AuthSession {
  user: User | null
  oauthEnabled: boolean
  emailEnabled: boolean
}

export const useAuthStore = defineStore('auth', () => {
  const user = ref<User | null>(null)
  const oauthEnabled = ref(false)
  const emailEnabled = ref(false)

  function apply(session: AuthSession) {
    user.value = session.user
    oauthEnabled.value = session.oauthEnabled
    emailEnabled.value = session.emailEnabled
  }

  async function refresh() {
    apply(await requestJSON<AuthSession>('/auth/session'))
  }

  async function signIn(email: string, password: string) {
    apply(await requestJSON<AuthSession>('/auth/password', { email, password }))
  }

  async function signOut() {
    await requestJSON<void>('/auth/logout', {})
    user.value = null
  }

  return { user, oauthEnabled, emailEnabled, apply, refresh, signIn, signOut }
})
