<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiDialog from '../../shared/ui/UiDialog.vue'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton, UiForm, UiInline, UiSubheading } from '../../shared/ui/elements'
import { accountApi } from './accountApi'
import { useAuthStore } from './authStore'

const open = defineModel<boolean>({ required: true })
const auth = useAuthStore()
const email = ref('')
const current = ref('')
const code = ref('')
const request = ref('')
const busy = ref(false)
const label = computed(() => t(auth.user?.loginEmail ? 'account.changeEmail' : 'account.bindEmail'))
const verifying = computed(() => !!request.value)

watch(open, (visible) => {
  if (visible) return
  email.value = ''
  current.value = ''
  code.value = ''
  request.value = ''
})

async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    if (verifying.value) {
      auth.apply(await accountApi.verifyEmail(request.value, code.value.trim()))
      open.value = false
      notify.success(t('account.emailSaved'))
    } else {
      const response = await accountApi.sendEmailCode(email.value, current.value)
      request.value = response.request
      current.value = ''
      notify.success(t('account.codeSent'))
    }
  } catch (error) {
    notify.error(messageFor(error, verifying.value ? 'account.verifyFailed' : 'account.sendFailed'))
  } finally {
    busy.value = false
  }
}

function startOver() {
  request.value = ''
  code.value = ''
}
</script>

<template>
  <UiDialog v-model="open" :label="label" :busy="busy">
    <UiSubheading>{{ label }}</UiSubheading>
    <UiForm @submit.prevent="submit">
      <UiBox :hidden="verifying">
        <UiInput
          id="binding-email"
          v-model="email"
          :label="t('account.newEmail')"
          type="email"
          autocomplete="email"
          :disabled="busy"
        />
        <UiBox :hidden="!auth.user?.hasPassword">
          <UiInput
            id="binding-current-password"
            v-model="current"
            :label="t('account.currentPassword')"
            type="password"
            autocomplete="current-password"
            :disabled="busy"
          />
        </UiBox>
      </UiBox>
      <UiBox :hidden="!verifying">
        <UiInline class="dialog-message">{{ t('account.codeInstructions', { email: email.trim() }) }}</UiInline>
        <UiInput
          id="binding-code"
          v-model="code"
          :label="t('account.verificationCode')"
          autocomplete="one-time-code"
          inputmode="numeric"
          maxlength="8"
          :disabled="busy"
        />
        <UiButton class="secondary-button" :disabled="busy" @click="startOver">{{ t('account.sendAnother') }}</UiButton>
      </UiBox>
      <UiBox class="form-actions">
        <UiButton class="ghost-button" :disabled="busy" @click="open = false">{{ t('common.cancel') }}</UiButton>
        <UiButton type="submit" :disabled="busy || (verifying ? !code.trim() : !email.trim())">
          {{ t(verifying ? 'account.verifyCode' : 'account.sendCode') }}
        </UiButton>
      </UiBox>
    </UiForm>
  </UiDialog>
</template>
