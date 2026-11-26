<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { useAuthStore } from '../features/auth/authStore'
import { useWorkbench } from '../features/sessions/workbench'
import { t } from '../shared/i18n'
import UiAvatar from '../shared/ui/UiAvatar.vue'
import UiDropdown from '../shared/ui/UiDropdown.vue'
import UiIcon from '../shared/ui/UiIcon.vue'
import UiIconButton from '../shared/ui/UiIconButton.vue'
import { UiBox, UiButton, UiHeader, UiHeading, UiImage, UiInline } from '../shared/ui/elements'

const props = defineProps<{ workbenchPage: boolean; sessionSelected: boolean; signingOut: boolean }>()
const emit = defineEmits<{ signOut: [] }>()
const auth = useAuthStore()
const route = useRoute()
const router = useRouter()
const {
  sessionsId, workspaceId, sessionsOpen, workspaceOpen, terminalOpen, sessionHeader,
  toggleSessions, toggleWorkspace, toggleTerminal,
} = useWorkbench()

const name = computed(() => auth.user?.name || auth.user?.email.split('@')[0] || t('common.account'))
const initials = computed(() => name.value.slice(0, 2).toUpperCase())
const title = computed(() => {
  switch (route.name) {
    case 'settings': return t('nav.settings')
    case 'account': return t('account.title')
    case 'admin-users': return t('adminUsers.title')
    case 'project-sandbox': return t('sandbox.manage')
    default: return sessionHeader.value?.title.value ?? t('nav.agent')
  }
})
const sessionsLabel = computed(() => t(sessionsOpen.value ? 'sessions.hide' : 'sessions.show'))
const workspaceLabel = computed(() => t(workspaceOpen.value ? 'workspace.hidePanel' : 'workspace.showPanel'))
const terminalLabel = computed(() => t(terminalOpen.value ? 'terminal.backToWorkspace' : 'terminal.open'))
const workbenchDisabledReason = computed(() => props.workbenchPage ? undefined : t('nav.workbenchControls'))
const workspaceDisabledReason = computed(() => workbenchDisabledReason.value ??
  (props.sessionSelected ? undefined : t('repository.selectSession')))

function openPage(close: () => void, name: 'account' | 'admin-users') {
  close()
  void router.push({ name })
}

function signOut(close: () => void) {
  close()
  emit('signOut')
}
</script>

<template>
  <UiHeader class="app-header">
    <UiBox class="app-header-start">
      <RouterLink
        class="app-brand"
        :to="{ name: 'sessions' }"
        :aria-label="t('nav.home')"
        :title="t('nav.home')"
      >
        <UiImage
          src="/shinona-logo.webp"
          alt=""
          width="28"
          height="28"
        />
        <UiInline class="app-brand-name">{{ t('nav.brand') }}</UiInline>
      </RouterLink>
      <UiIconButton
        icon="layout-left-line"
        :label="sessionsLabel"
        :title="workbenchDisabledReason ?? sessionsLabel"
        :aria-description="workbenchDisabledReason"
        :disabled="!workbenchPage"
        :aria-expanded="sessionsOpen"
        :aria-controls="workbenchPage ? sessionsId : undefined"
        @click="toggleSessions"
      />
    </UiBox>
    <UiBox class="app-header-center">
      <UiHeading v-if="workbenchPage" class="app-header-title" :title="title">{{ title }}</UiHeading>
      <UiInline v-else class="app-header-title" :title="title">{{ title }}</UiInline>
    </UiBox>
    <UiBox class="app-header-end">
      <UiIconButton
        icon="layout-right-line"
        :label="workspaceLabel"
        :title="workspaceDisabledReason ?? workspaceLabel"
        :aria-description="workspaceDisabledReason"
        :disabled="!sessionSelected"
        :aria-expanded="workspaceOpen"
        :aria-controls="workbenchPage ? workspaceId : undefined"
        @click="toggleWorkspace"
      />
      <UiIconButton
        icon="terminal-line"
        :label="terminalLabel"
        :title="workspaceDisabledReason ?? terminalLabel"
        :aria-description="workspaceDisabledReason"
        :disabled="!sessionSelected"
        :aria-pressed="terminalOpen"
        :aria-controls="workbenchPage ? workspaceId : undefined"
        @click="toggleTerminal"
      />
      <RouterLink
        class="icon-button app-header-link"
        :to="{ name: 'settings' }"
        :aria-label="t('nav.settings')"
        :title="t('nav.settings')"
      >
        <UiIcon name="settings-3-line" />
      </RouterLink>
      <UiDropdown class="header-account" :label="t('common.account')" boundary=".app-shell">
        <template #trigger>
          <UiAvatar :src="auth.user?.avatarUrl" :initials="initials" />
        </template>
        <template #default="{ close }">
          <UiInline class="account-description">{{ auth.user?.email || name }}</UiInline>
          <UiButton class="menu-item" @click="openPage(close, 'account')">
            <UiIcon name="user-settings-line" /> {{ t('account.title') }}
          </UiButton>
          <UiButton v-if="auth.user?.admin" class="menu-item" @click="openPage(close, 'admin-users')">
            <UiIcon name="group-line" /> {{ t('adminUsers.title') }}
          </UiButton>
          <UiButton class="menu-item" :disabled="signingOut" @click="signOut(close)">
            <UiIcon name="logout-box-r-line" /> {{ t('auth.signOut') }}
          </UiButton>
        </template>
      </UiDropdown>
    </UiBox>
  </UiHeader>
</template>
