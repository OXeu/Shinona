<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import type { MessageKey } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiAvatar from '../../shared/ui/UiAvatar.vue'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import UiMoreMenu from '../../shared/ui/UiMoreMenu.vue'
import UiSearchField from '../../shared/ui/UiSearchField.vue'
import { UiBox, UiButton, UiForm, UiHeader, UiHeading, UiInline, UiList, UiListItem, UiSubheading } from '../../shared/ui/elements'
import { accountApi } from './accountApi'
import { useAuthStore } from './authStore'
import type { User } from './authStore'

const auth = useAuthStore()
const search = ref('')
const appliedSearch = ref('')
const offset = ref(0)
const limit = ref(25)
const users = ref<User[]>([])
const total = ref(0)
const loading = ref(false)
const loaded = ref(false)
const busy = ref(false)
const createOpen = ref(false)
const accessOpen = ref(false)
type AccountAction = 'admin' | 'disabled' | 'delete'
const action = ref<{ user: User; field: AccountAction }>()
const createEmail = ref('')
const createName = ref('')
const createPassword = ref('')
let request: AbortController | undefined

const pageLabel = computed(() => t('adminUsers.page', {
  first: total.value ? offset.value + 1 : 0,
  last: offset.value + users.value.length,
  total: total.value,
}))
const actionLabel = computed(() => {
  if (!action.value) return t('adminUsers.access')
  return t(accessLabel(action.value.user, action.value.field))
})
const actionTarget = computed(() => action.value ? userName(action.value.user) : '')
const deleting = computed(() => action.value?.field === 'delete')

function userName(user: User) {
  return user.name || user.email || user.id
}

function accessLabel(user: User, field: AccountAction): MessageKey {
  if (field === 'delete') return 'adminUsers.delete'
  if (field === 'admin') return user.admin ? 'adminUsers.removeAdmin' : 'adminUsers.makeAdmin'
  return user.disabled ? 'adminUsers.enable' : 'adminUsers.disable'
}

async function load(next = offset.value) {
  request?.abort()
  const task = new AbortController()
  request = task
  loading.value = true
  try {
    const page = await accountApi.listUsers(appliedSearch.value, next, task.signal)
    if (task.signal.aborted) return
    users.value = page.users
    total.value = page.total
    limit.value = page.limit
    offset.value = next
    loaded.value = true
  } catch (error) {
    if (!task.signal.aborted) notify.error(messageFor(error, 'adminUsers.loadFailed'))
  } finally {
    if (!task.signal.aborted) loading.value = false
  }
}

async function searchUsers() {
  appliedSearch.value = search.value.trim()
  await load(0)
}

function chooseAction(user: User, field: AccountAction) {
  action.value = { user, field }
  accessOpen.value = true
}

async function updateAccess() {
  const selected = action.value
  if (busy.value || !selected) return
  busy.value = true
  try {
    if (selected.field === 'delete') {
      await accountApi.deleteUser(selected.user.id)
      users.value = users.value.filter((user) => user.id !== selected.user.id)
      total.value = Math.max(0, total.value - 1)
      const next = Math.min(offset.value, Math.max(0, Math.ceil(total.value / limit.value) - 1) * limit.value)
      accessOpen.value = false
      notify.success(t('adminUsers.deleted'))
      await load(next)
      return
    }
    const change = selected.field === 'admin' ? { admin: !selected.user.admin } : { disabled: !selected.user.disabled }
    const updated = await accountApi.updateAccess(selected.user.id, change)
    users.value = users.value.map((user) => user.id === updated.id ? updated : user)
    accessOpen.value = false
    notify.success(t('adminUsers.accessSaved'))
  } catch (error) {
    notify.error(messageFor(error, selected.field === 'delete' ? 'adminUsers.deleteFailed' : 'adminUsers.saveFailed'))
  } finally {
    busy.value = false
  }
}

async function create() {
  if (busy.value) return
  busy.value = true
  try {
    const user = await accountApi.createUser(createEmail.value, createPassword.value, createName.value)
    createOpen.value = false
    search.value = user.email
    notify.success(t('adminUsers.created'))
    await searchUsers()
  } catch (error) {
    notify.error(messageFor(error, 'adminUsers.saveFailed'))
  } finally {
    busy.value = false
  }
}

watch(createOpen, (open) => {
  if (open) return
  createEmail.value = ''
  createName.value = ''
  createPassword.value = ''
})
onMounted(() => load())
onUnmounted(() => request?.abort())
</script>

<template>
  <UiBox class="page admin-users">
    <UiHeader class="admin-users-header">
      <UiHeading>{{ t('adminUsers.title') }}</UiHeading>
      <UiButton :disabled="busy" @click="createOpen = true">{{ t('adminUsers.create') }}</UiButton>
    </UiHeader>
    <UiForm class="admin-user-search ui-form--custom" @submit.prevent="searchUsers">
      <UiSearchField v-model="search" :label="t('adminUsers.search')" />
      <UiButton class="secondary-button" :disabled="loading" type="submit">{{ t('adminUsers.searchAction') }}</UiButton>
    </UiForm>
    <UiInline class="admin-user-description" :hidden="!loaded || loading || !!users.length">{{ t('adminUsers.empty') }}</UiInline>
    <UiList class="admin-user-list" :aria-label="t('adminUsers.title')" :aria-busy="loading">
      <UiListItem v-for="user in users" :key="user.id" class="admin-user-row">
        <UiAvatar :src="user.avatarUrl" :initials="userName(user).slice(0, 2).toUpperCase()" />
        <UiBox class="admin-user-identity">
          <UiInline>{{ userName(user) }}</UiInline>
          <UiInline
            class="admin-user-description"
            :hidden="!user.name || !user.email"
          >
            {{ user.email }}
          </UiInline>
        </UiBox>
        <UiInline class="admin-user-description" :hidden="!user.disabled">{{ t('adminUsers.disabled') }}</UiInline>
        <UiInline class="admin-user-description">{{ t(user.admin ? 'adminUsers.admin' : 'adminUsers.member') }}</UiInline>
        <UiBox :hidden="user.id === auth.user?.id">
          <UiMoreMenu :label="t('adminUsers.access')" boundary=".app-main" :disabled="busy || loading">
            <template #default="{ close }">
              <UiButton class="menu-item" @click="close(); chooseAction(user, 'admin')">{{ t(accessLabel(user, 'admin')) }}</UiButton>
              <UiButton class="menu-item" @click="close(); chooseAction(user, 'disabled')">{{ t(accessLabel(user, 'disabled')) }}</UiButton>
              <UiButton class="menu-item danger-text" @click="close(); chooseAction(user, 'delete')">{{ t('adminUsers.delete') }}</UiButton>
            </template>
          </UiMoreMenu>
        </UiBox>
      </UiListItem>
    </UiList>
    <UiBox class="admin-users-header" :hidden="!total">
      <UiInline class="admin-user-description">{{ pageLabel }}</UiInline>
      <UiBox class="admin-user-pagination">
        <UiButton class="secondary-button" :disabled="loading || !offset" @click="load(offset - limit)">{{ t('adminUsers.previous') }}</UiButton>
        <UiButton
          class="secondary-button"
          :disabled="loading || offset + limit >= total"
          @click="load(offset + limit)"
        >
          {{ t('adminUsers.next') }}
        </UiButton>
      </UiBox>
    </UiBox>

    <UiDialog v-model="createOpen" :label="t('adminUsers.create')" :busy="busy">
      <UiSubheading>{{ t('adminUsers.create') }}</UiSubheading>
      <UiForm @submit.prevent="create">
        <UiInput
          id="admin-create-name"
          v-model="createName"
          :label="t('account.nickname')"
          :disabled="busy"
        />
        <UiInput
          id="admin-create-email"
          v-model="createEmail"
          :label="t('auth.email')"
          type="email"
          :disabled="busy"
        />
        <UiInput
          id="admin-create-password"
          v-model="createPassword"
          :label="t('account.newPassword')"
          type="password"
          autocomplete="new-password"
          :disabled="busy"
        />
        <UiBox class="form-actions">
          <UiButton class="ghost-button" :disabled="busy" @click="createOpen = false">{{ t('common.cancel') }}</UiButton>
          <UiButton type="submit" :disabled="busy || !createEmail.trim() || !createPassword">{{ t('adminUsers.create') }}</UiButton>
        </UiBox>
      </UiForm>
    </UiDialog>
    <UiDialog v-model="accessOpen" :label="actionLabel" :busy="busy">
      <UiSubheading>{{ actionLabel }}</UiSubheading>
      <UiInline class="dialog-message">
        {{ t(deleting ? 'adminUsers.confirmDelete' : 'adminUsers.confirmAccess', { user: actionTarget }) }}
      </UiInline>
      <UiBox class="form-actions">
        <UiButton class="ghost-button" :disabled="busy" @click="accessOpen = false">{{ t('common.cancel') }}</UiButton>
        <UiButton :class="{ 'danger-text': deleting, 'ghost-button': deleting }" :disabled="busy" @click="updateAccess">
          {{ actionLabel }}
        </UiButton>
      </UiBox>
    </UiDialog>
  </UiBox>
</template>

<style scoped lang="less">
.admin-users { display: grid; gap: 16px; }
.admin-users-header, .admin-user-search { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px; }
.admin-user-search :deep(.search-field) { flex: 1; }
.admin-user-list { margin: 0; padding: 0; list-style: none; }
.admin-user-row { display: flex; align-items: center; gap: 10px; padding-block: 8px; border-bottom: 1px solid var(--border); }
.admin-user-identity { flex: 1; min-width: 0; overflow-wrap: anywhere; }
.admin-user-identity > span { display: block; }
.admin-user-description { color: var(--muted); font-size: 0.875rem; }
.admin-user-pagination { display: flex; gap: 4px; }
.danger-text { color: var(--danger); }
</style>
