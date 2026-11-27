<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { messageFor } from '../../shared/errors'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import UiInput from '../../shared/ui/UiInput.vue'
import UiLanguagePicker from '../../shared/ui/UiLanguagePicker.vue'
import { UiAnchor, UiBox, UiButton, UiForm, UiHeader, UiHeading, UiImage } from '../../shared/ui/elements'
import { useAuthStore } from './authStore'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()
const email = ref('')
const password = ref('')
const signingIn = ref(false)

onMounted(async () => {
  if (route.query.oauth === 'failed') {
    notify.error(t('auth.oauthFailed'))
    await router.replace({ name: 'login' })
  }
  try {
    await auth.refresh()
    if (auth.user) await router.replace({ name: 'sessions' })
  } catch {
    notify.error(t('auth.connectionFailed'))
  }
})

async function signIn() {
  if (signingIn.value) return
  if (!email.value.trim() || !password.value) {
    notify.error(t('auth.enterCredentials'))
    return
  }
  signingIn.value = true
  try {
    await auth.signIn(email.value, password.value)
    notify.success(t('auth.signedIn'))
    await router.replace({ name: 'sessions' })
  } catch (error) {
    notify.error(messageFor(error, 'auth.signInFailed'))
  } finally {
    password.value = ''
    signingIn.value = false
  }
}
</script>

<template>
  <UiBox class="page login-page">
    <UiHeader class="login-header">
      <UiImage
        class="login-logo"
        src="/shinona-logo.webp"
        alt=""
        width="48"
        height="48"
      />
      <UiBox class="login-heading-row">
        <UiHeading>{{ t('auth.signInHeading') }}</UiHeading>
        <UiLanguagePicker class="login-language" boundary=".login-heading-row" text-trigger />
      </UiBox>
    </UiHeader>
    <UiForm @submit.prevent="signIn">
      <UiInput
        id="email"
        v-model="email"
        :label="t('auth.email')"
        type="email"
        autocomplete="username"
      />
      <UiInput
        id="password"
        v-model="password"
        :label="t('auth.password')"
        type="password"
        autocomplete="current-password"
      />
      <UiBox class="form-actions">
        <UiAnchor
          class="ghost-button"
          href="/auth/oauth/start"
          :hidden="!auth.oauthEnabled"
        >
          {{ t('auth.oauthSignIn') }}
        </UiAnchor>
        <UiButton type="submit" :disabled="signingIn">
          {{ t(signingIn ? 'auth.signingIn' : 'auth.signIn') }}
        </UiButton>
      </UiBox>
    </UiForm>
  </UiBox>
</template>

<style lang="less" scoped>
.login-header {
  display: grid;
  gap: 0.75rem;
  margin-block-end: 1.25rem;
}

.login-page .form-actions {
  justify-content: flex-start;
}

.form-actions > :is(button, .ghost-button) {
  border-radius: var(--radius-full);
}

.form-actions > button[type='submit'] {
  min-inline-size: 5.5rem;
  margin-inline-start: auto;
}

.login-logo {
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  object-fit: cover;
  transition: transform 180ms cubic-bezier(0.34, 1.56, 0.64, 1);
}

.login-logo:hover {
  transform: translateY(-2px) rotate(-4deg);
}

@media (prefers-reduced-motion: reduce) {
  .login-logo { transition: none; }
  .login-logo:hover { transform: none; }
}

.login-heading-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem 1rem;
}

.login-heading-row h1 { font-size: 1.5rem; }
.login-language { margin-inline-start: auto; }

.login-page :deep(.input:is(:autofill, :-webkit-autofill)) {
  box-shadow: inset 0 0 0 1000px var(--surface);
  -webkit-text-fill-color: var(--foreground);
  caret-color: var(--foreground);
  transition-property: border-color, outline-color;
}
</style>
