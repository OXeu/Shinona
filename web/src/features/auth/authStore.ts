import { defineStore } from 'pinia'
import { ref } from 'vue'

import { requestJSON } from '../../shared/http'

interface User {
  id: string
  email: string
  avatarUrl?: string
}

interface AuthSession {
  user: User | null
  oauthEnabled: boolean
}

export const useAuthStore = defineStore('auth', () => {
  const user = ref<User | null>(null)
  const oauthEnabled = ref(false)

  function apply(session: AuthSession) {
    user.value = session.user
    oauthEnabled.value = session.oauthEnabled
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

  return { user, oauthEnabled, refresh, signIn, signOut }
})
