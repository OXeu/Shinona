<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton, UiForm, UiInline, UiSubheading } from '../../shared/ui/elements'
import { accountApi } from './accountApi'
import { useAuthStore } from './authStore'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()
const status = ref<boolean | null>(null)
const open = ref(false)
const busy = ref(false)
const password = ref('')
const label = computed(() => t(status.value ? 'account.oauthUnlink' : 'account.oauthLink'))
const actionAvailable = computed(() =>
  status.value ? !!auth.user?.hasPassword && !!auth.user.loginEmail : auth.oauthEnabled)
const request = new AbortController()

async function handleCallback() {
  const { oauthLink, ...query } = route.query
  switch (oauthLink) {
    case 'linked':
      notify.success(t('account.oauthLinkSaved'))
      break
    case 'in-use':
      notify.error(t('account.oauthInUse'))
      break
    case 'failed':
      notify.error(t('account.oauthLinkFailed'))
      break
    default:
      return
  }
  await router.replace({ query })
}

onMounted(async () => {
  await handleCallback()
  try {
    status.value = (await accountApi.oauthStatus(request.signal)).linked
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, 'account.oauthLoadFailed'))
  }
})
onUnmounted(() => request.abort())

watch(open, (visible) => {
  if (!visible) password.value = ''
})

async function submit() {
  if (busy.value || !actionAvailable.value) return
  busy.value = true
  try {
    if (status.value) {
      const session = await accountApi.unlinkOAuth(password.value)
      if (request.signal.aborted) return
      auth.apply(session)
      status.value = false
      open.value = false
      notify.success(t('account.oauthUnlinkSaved'))
    } else {
      const { url } = await accountApi.startOAuthLink(password.value)
      if (!request.signal.aborted) location.assign(url)
    }
  } catch (error) {
    if (!request.signal.aborted) notify.error(messageFor(error, status.value ? 'account.oauthUnlinkFailed' : 'account.oauthLinkFailed'))
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UiBox class="oauth-account-row" :hidden="status === null">
    <UiBox>
      <UiInline>{{ t('auth.oauth') }}</UiInline>
      <UiInline class="oauth-account-caption">
        {{ t(status ? 'account.oauthLinked' : 'account.oauthUnlinked') }}
      </UiInline>
      <UiInline class="oauth-account-caption" :hidden="actionAvailable">
        {{ t(status ? 'account.oauthNeedsPassword' : 'account.oauthDisabled') }}
      </UiInline>
    </UiBox>
    <UiButton class="secondary-button" :disabled="!actionAvailable" @click="open = true">{{ label }}</UiButton>
  </UiBox>
  <UiDialog v-model="open" :label="label" :busy="busy">
    <UiSubheading>{{ label }}</UiSubheading>
    <UiInline class="dialog-message">
      {{ t(status ? 'account.oauthUnlinkDetails' : 'account.oauthLinkDetails') }}
    </UiInline>
    <UiForm @submit.prevent="submit">
      <UiBox :hidden="!auth.user?.hasPassword">
        <UiInput
          id="oauth-current-password"
          v-model="password"
          :label="t('account.currentPassword')"
          autocomplete="current-password"
          type="password"
          :disabled="busy"
        />
      </UiBox>
      <UiBox class="form-actions">
        <UiButton class="ghost-button" :disabled="busy" @click="open = false">{{ t('common.cancel') }}</UiButton>
        <UiButton type="submit" :disabled="busy || (!!auth.user?.hasPassword && !password)">{{ label }}</UiButton>
      </UiBox>
    </UiForm>
  </UiDialog>
</template>

<style lang="less" scoped>
.oauth-account-row { display: flex; justify-content: space-between; align-items: baseline; gap: 12px; }
.oauth-account-caption { display: block; color: var(--muted); font-size: 0.875rem; margin-top: 4px; }
</style>
