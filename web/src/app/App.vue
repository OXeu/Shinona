<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'

import { useAuthStore } from '../features/auth/authStore'
import { useSessionStore } from '../features/sessions/sessionStore'
import { t } from '../shared/i18n'
import { notify, Toaster } from '../shared/notifications'
import UiSplitPanel from '../shared/ui/UiSplitPanel.vue'
import { UiMain } from '../shared/ui/elements'
import { useMediaQuery } from '../shared/ui/useMediaQuery'
import AppNav from './AppNav.vue'

const auth = useAuthStore()
const sessions = useSessionStore()
const route = useRoute()
const router = useRouter()
const signingOut = ref(false)
const navSize = ref(16.7)
const navCollapsed = ref(false)
const compact = useMediaQuery('(max-width: 58rem)')
const showNav = computed(() => route.name !== 'login' && !!auth.user)
const collapsed = computed(() => navCollapsed.value || compact.value)

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
  <UiSplitPanel
    v-model="navSize"
    class="app-shell"
    :start-min="154"
    :end-min="610"
    :start-collapsed="!showNav || collapsed"
    :collapsed-start-size="showNav ? (compact ? 54 : 64) : 0"
    :label="t('nav.resize')"
  >
    <template #start>
      <AppNav
        :collapsed="collapsed"
        :compact="compact"
        :signing-out="signingOut"
        @toggle="navCollapsed = !navCollapsed"
        @sign-out="signOut"
      />
    </template>
    <template #end>
      <UiMain class="app-main">
        <RouterView />
      </UiMain>
    </template>
  </UiSplitPanel>
  <Toaster position="top-right" theme="system" />
</template>
