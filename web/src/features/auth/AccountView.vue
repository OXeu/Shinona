<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiAvatar from '../../shared/ui/UiAvatar.vue'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiImagePicker from '../../shared/ui/UiImagePicker.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton, UiForm, UiHeading, UiInline, UiSection, UiSubheading } from '../../shared/ui/elements'
import { accountApi } from './accountApi'
import { useAuthStore } from './authStore'
import EmailBindingDialog from './EmailBindingDialog.vue'
import OAuthAccountSettings from './OAuthAccountSettings.vue'

const auth = useAuthStore()
const nickname = ref(auth.user?.name ?? '')
const profileBusy = ref(false)
const avatar = ref('')
const avatarType = ref('')
const removeAvatar = ref(false)
const emailOpen = ref(false)
const passwordOpen = ref(false)
const passwordBusy = ref(false)
const current = ref('')
const password = ref('')
const confirmation = ref('')
const passwordLabel = computed(() => t(auth.user?.hasPassword ? 'account.changePassword' : 'account.setPassword'))
const avatarSource = computed(() => {
  if (avatar.value) return 'data:' + avatarType.value + ';base64,' + avatar.value
  return removeAvatar.value ? undefined : auth.user?.avatarUrl
})
const initials = computed(() => (nickname.value.trim() || auth.user?.email || '?').slice(0, 2).toUpperCase())
const profileDirty = computed(() => nickname.value !== (auth.user?.name ?? '') || !!avatar.value || removeAvatar.value)

async function selectAvatar(files: File[]) {
  const file = files.at(0)
  if (!file) return
  if (file.size > 512 * 1024 || !['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) {
    notify.error(t('account.invalidAvatar'))
    return
  }
  try {
    const bytes = new Uint8Array(await file.arrayBuffer())
    const parts: string[] = []
    for (let offset = 0; offset < bytes.length; offset += 8192) {
      parts.push(String.fromCharCode(...bytes.subarray(offset, offset + 8192)))
    }
    avatar.value = btoa(parts.join(''))
    avatarType.value = file.type
    removeAvatar.value = false
  } catch {
    notify.error(t('account.invalidAvatar'))
  }
}

function resetAvatar() {
  avatar.value = ''
  removeAvatar.value = !!auth.user?.hasAvatar
}

async function saveProfile() {
  if (profileBusy.value || !profileDirty.value) return
  profileBusy.value = true
  try {
    const response = await accountApi.updateProfile({
      name: nickname.value,
      avatar: avatar.value || undefined,
      removeAvatar: removeAvatar.value,
    })
    auth.apply(response)
    nickname.value = response.user?.name ?? ''
    avatar.value = ''
    removeAvatar.value = false
    notify.success(t('account.profileSaved'))
  } catch (error) {
    notify.error(messageFor(error, 'account.saveFailed'))
  } finally {
    profileBusy.value = false
  }
}

watch(passwordOpen, (open) => {
  if (open) return
  current.value = ''
  password.value = ''
  confirmation.value = ''
})

async function savePassword() {
  if (passwordBusy.value) return
  if (password.value !== confirmation.value) {
    notify.error(t('account.passwordMismatch'))
    return
  }
  passwordBusy.value = true
  try {
    auth.apply(await accountApi.changePassword(current.value, password.value))
    passwordOpen.value = false
    notify.success(t('account.passwordSaved'))
  } catch (error) {
    notify.error(messageFor(error, 'account.saveFailed'))
  } finally {
    passwordBusy.value = false
  }
}
</script>

<template>
  <UiBox class="page account-page">
    <UiHeading>{{ t('account.title') }}</UiHeading>
    <UiSection class="account-section" :aria-label="t('account.profile')">
      <UiSubheading>{{ t('account.profile') }}</UiSubheading>
      <UiForm @submit.prevent="saveProfile">
        <UiBox class="account-avatar-controls">
          <UiAvatar :src="avatarSource" :initials="initials" />
          <UiImagePicker
            :label="t('account.chooseAvatar')"
            :disabled="profileBusy"
            :multiple="false"
            @files="selectAvatar"
          />
          <UiButton
            class="secondary-button"
            :hidden="!avatar && (!auth.user?.hasAvatar || removeAvatar)"
            :disabled="profileBusy"
            @click="resetAvatar"
          >
            {{ t('account.resetAvatar') }}
          </UiButton>
        </UiBox>
        <UiInput
          id="profile-nickname"
          v-model="nickname"
          :label="t('account.nickname')"
          autocomplete="nickname"
          :disabled="profileBusy"
        />
        <UiBox class="form-actions">
          <UiButton type="submit" :disabled="profileBusy || !profileDirty">{{ t('settings.save') }}</UiButton>
        </UiBox>
      </UiForm>
    </UiSection>

    <UiSection class="account-section" :aria-label="t('account.security')">
      <UiSubheading>{{ t('account.security') }}</UiSubheading>
      <UiBox class="account-detail">
        <UiBox>
          <UiInline>{{ t('account.signInEmail') }}</UiInline>
          <UiInline class="account-hint">{{ auth.user?.loginEmail || t('account.emailNotBound') }}</UiInline>
          <UiInline class="account-hint" :hidden="auth.emailEnabled">{{ t('account.emailDisabled') }}</UiInline>
        </UiBox>
        <UiButton class="secondary-button" :disabled="!auth.emailEnabled" @click="emailOpen = true">
          {{ t(auth.user?.loginEmail ? 'account.changeEmail' : 'account.bindEmail') }}
        </UiButton>
      </UiBox>
      <UiBox class="account-detail">
        <UiBox>
          <UiInline>{{ t('auth.password') }}</UiInline>
          <UiInline class="account-hint" :hidden="!!auth.user?.loginEmail">{{ t('account.bindEmailFirst') }}</UiInline>
        </UiBox>
        <UiButton class="secondary-button" :disabled="!auth.user?.loginEmail" @click="passwordOpen = true">{{ passwordLabel }}</UiButton>
      </UiBox>
      <OAuthAccountSettings />
    </UiSection>
    <EmailBindingDialog v-model="emailOpen" />
    <UiDialog v-model="passwordOpen" :label="passwordLabel" :busy="passwordBusy">
      <UiSubheading>{{ passwordLabel }}</UiSubheading>
      <UiForm @submit.prevent="savePassword">
        <UiBox :hidden="!auth.user?.hasPassword">
          <UiInput
            id="profile-current-password"
            v-model="current"
            :label="t('account.currentPassword')"
            type="password"
            autocomplete="current-password"
            :disabled="passwordBusy"
          />
        </UiBox>
        <UiInput
          id="profile-new-password"
          v-model="password"
          :label="t('account.newPassword')"
          type="password"
          autocomplete="new-password"
          :disabled="passwordBusy"
        />
        <UiInput
          id="profile-confirm-password"
          v-model="confirmation"
          :label="t('account.confirmPassword')"
          type="password"
          autocomplete="new-password"
          :disabled="passwordBusy"
        />
        <UiBox class="form-actions">
          <UiButton class="ghost-button" :disabled="passwordBusy" @click="passwordOpen = false">{{ t('common.cancel') }}</UiButton>
          <UiButton type="submit" :disabled="passwordBusy || !password || !confirmation">{{ t('settings.save') }}</UiButton>
        </UiBox>
      </UiForm>
    </UiDialog>
  </UiBox>
</template>

<style scoped lang="less">
.account-page { display: grid; gap: 20px; }
.account-section { display: grid; gap: 12px; }
.account-section + .account-section { border-top: 1px solid var(--border); padding-top: 20px; }
.account-avatar-controls { display: flex; align-items: center; gap: 8px; }
.account-avatar-controls :deep(.ui-avatar) { width: 56px; height: 56px; font-size: 1rem; }
.account-detail { display: flex; justify-content: space-between; align-items: baseline; gap: 12px; }
.account-hint { display: block; color: var(--muted); font-size: 0.875rem; margin-top: 4px; overflow-wrap: anywhere; }
</style>
