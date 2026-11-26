<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'

import { useAuthStore } from '../features/auth/authStore'
import { useSessionStore } from '../features/sessions/sessionStore'
import { provideWorkbench } from '../features/sessions/workbench'
import { t } from '../shared/i18n'
import { notify } from '../shared/notifications'
import UiToaster from '../shared/ui/UiToaster.vue'
import { UiBox, UiMain } from '../shared/ui/elements'
import AppHeader from './AppHeader.vue'

const auth = useAuthStore()
const sessions = useSessionStore()
const route = useRoute()
const router = useRouter()
const signingOut = ref(false)
const showHeader = computed(() => route.name !== 'login' && !!auth.user)
const workbenchPage = computed(() => route.name === 'sessions' || route.name === 'session')
const sessionId = computed(() => route.name === 'session' ? String(route.params.id) : undefined)
provideWorkbench(() => workbenchPage.value, () => sessionId.value)

async function signOut() {
  if (signingOut.value) return
  signingOut.value = true
  try {
    await auth.signOut()
    sessions.useAccount(undefined)
    notify.success(t('auth.signedOut'))
    await router.replace({ name: 'login' })
  } catch {
    notify.error(t('auth.signOutFailed'))
  } finally {
    signingOut.value = false
  }
}
</script>

<template>
  <UiBox class="app-shell">
    <AppHeader
      v-if="showHeader"
      :workbench-page="workbenchPage"
      :session-selected="!!sessionId"
      :signing-out="signingOut"
      @sign-out="signOut"
    />
    <UiMain class="app-main">
      <RouterView />
    </UiMain>
  </UiBox>
  <UiToaster :below-header="showHeader" />
</template>
