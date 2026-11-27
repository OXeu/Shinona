<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

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
const router = useRouter()
const {
  sessionsId, workspaceId, sessionsOpen, workspaceOpen, sessionHeader,
  toggleSessions, toggleWorkspace,
} = useWorkbench()

const name = computed(() => auth.user?.name || auth.user?.email.split('@')[0] || t('common.account'))
const initials = computed(() => name.value.slice(0, 2).toUpperCase())
const title = computed(() => sessionHeader.value?.title.value ?? t('nav.agent'))
const sessionsLabel = computed(() => t(sessionsOpen.value ? 'sessions.hide' : 'sessions.show'))
const workspaceLabel = computed(() => t(workspaceOpen.value ? 'workspace.hidePanel' : 'workspace.showPanel'))
const workspaceDisabledReason = computed(() => props.sessionSelected ? undefined : t('repository.selectSession'))

function openPage(close: () => void, name: 'settings' | 'account' | 'admin-users') {
  close()
  void router.push({ name })
}

function goBack() {
  const back = (window.history.state as { back?: unknown } | null)?.back
  if (typeof back === 'string' && back && back !== '/login') router.back()
  else void router.push({ name: 'sessions' })
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
        v-if="workbenchPage"
        icon="layout-left-line"
        :label="sessionsLabel"
        :aria-expanded="sessionsOpen"
        :aria-controls="sessionsId"
        @click="toggleSessions"
      />
    </UiBox>
    <UiBox class="app-header-center">
      <UiHeading v-if="workbenchPage" class="app-header-title" :title="title">{{ title }}</UiHeading>
    </UiBox>
    <UiBox class="app-header-end">
      <UiIconButton
        v-if="workbenchPage"
        icon="layout-right-line"
        :label="workspaceLabel"
        :title="workspaceDisabledReason ?? workspaceLabel"
        :aria-description="workspaceDisabledReason"
        :disabled="!sessionSelected"
        :aria-expanded="workspaceOpen"
        :aria-controls="workspaceId"
        @click="toggleWorkspace"
      />
      <UiDropdown class="header-account" :label="t('common.account')" boundary=".app-shell">
        <template #trigger>
          <UiAvatar :src="auth.user?.avatarUrl" :initials="initials" />
        </template>
        <template #default="{ close }">
          <UiInline class="account-description">{{ auth.user?.email || name }}</UiInline>
          <UiButton class="menu-item" @click="openPage(close, 'account')">
            <UiIcon name="user-settings-line" /> {{ t('account.title') }}
          </UiButton>
          <UiButton class="menu-item" @click="openPage(close, 'settings')">
            <UiIcon name="settings-3-line" /> {{ t('nav.settings') }}
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
