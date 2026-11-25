<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { useAuthStore } from '../features/auth/authStore'
import { t } from '../shared/i18n'
import UiAvatar from '../shared/ui/UiAvatar.vue'
import UiDropdown from '../shared/ui/UiDropdown.vue'
import UiIcon from '../shared/ui/UiIcon.vue'
import { UiBox, UiButton, UiImage, UiInline, UiNav } from '../shared/ui/elements'

const props = defineProps<{ collapsed: boolean; compact: boolean; signingOut: boolean }>()
const emit = defineEmits<{ toggle: []; signOut: [] }>()
const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const links = [
  { name: 'sessions', key: 'nav.agent', icon: 'chat-3-line', routes: ['sessions', 'session'] },
  { name: 'settings', key: 'nav.settings', icon: 'settings-3-line', routes: ['settings'] },
] as const
const name = computed(() => auth.user?.name || auth.user?.email.split('@')[0] || t('common.account'))
const initials = computed(() => name.value.slice(0, 2).toUpperCase())
const collapseLabel = computed(() => t(props.collapsed ? 'nav.expandNavigation' : 'nav.collapseNavigation'))
</script>

<template>
  <UiNav class="app-nav" :class="{ 'app-nav--collapsed': collapsed }" :aria-label="t('nav.main')">
    <RouterLink class="nav-brand" :to="{ name: 'sessions' }" :aria-label="t('nav.home')">
      <UiImage
        class="brand-icon"
        src="/shinona-logo.webp"
        alt=""
        width="32"
        height="32"
      />
      <UiInline class="nav-caption">Shinona</UiInline>
    </RouterLink>

    <UiBox class="nav-items">
      <RouterLink
        v-for="link in links"
        :key="link.name"
        :to="{ name: link.name }"
        class="nav-item"
        :class="{ 'nav-item--current': link.routes.some((name) => name === route.name) }"
        :aria-label="t(link.key)"
        :title="t(link.key)"
      >
        <UiIcon :name="link.icon" />
        <UiInline class="nav-caption">{{ t(link.key) }}</UiInline>
      </RouterLink>
    </UiBox>

    <UiBox class="nav-bottom">
      <UiDropdown
        :label="t('common.account')"
        :boundary="collapsed ? '.app-shell' : undefined"
        align="start"
        side="top"
      >
        <template #trigger>
          <UiBox class="nav-item nav-account">
            <UiAvatar :src="auth.user?.avatarUrl" :initials="initials" />
            <UiInline class="nav-caption">{{ name }}</UiInline>
          </UiBox>
        </template>
        <template #default="{ close }">
          <UiInline class="account-description">{{ auth.user?.email || name }}</UiInline>
          <UiButton class="menu-item" @click="close(); router.push({ name: 'account' })">
            <UiIcon name="user-settings-line" /> {{ t('account.title') }}
          </UiButton>
          <UiButton class="menu-item" :hidden="!auth.user?.admin" @click="close(); router.push({ name: 'admin-users' })">
            <UiIcon name="group-line" /> {{ t('adminUsers.title') }}
          </UiButton>
          <UiButton class="menu-item" :disabled="signingOut" @click="close(); emit('signOut')">
            <UiIcon name="logout-box-r-line" /> {{ t('auth.signOut') }}
          </UiButton>
        </template>
      </UiDropdown>
      <UiButton
        class="nav-item"
        :hidden="compact"
        :aria-label="collapseLabel"
        :title="collapseLabel"
        :aria-expanded="!collapsed"
        @click="emit('toggle')"
      >
        <UiIcon :name="collapsed ? 'menu-unfold-line' : 'menu-fold-line'" />
        <UiInline class="nav-caption">{{ t(collapsed ? 'nav.expand' : 'nav.collapse') }}</UiInline>
      </UiButton>
    </UiBox>
  </UiNav>
</template>
