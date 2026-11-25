<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../../shared/i18n'
import UiInput from '../../shared/ui/UiInput.vue'
import { UiBox, UiButton } from '../../shared/ui/elements'
import type { SettingField } from './settingsApi'

const props = defineProps<{ field: SettingField; label: string; resetLabel?: string; draft?: string | null; disabled: boolean }>()
const emit = defineEmits<{ edit: [value: string]; clear: []; reset: [] }>()
const secret = computed(() => props.field.value === null)
const value = computed(() => props.draft === undefined ? props.field.value ?? '' : props.draft ?? '')
const placeholder = computed(() => {
  if (props.draft === null) return props.resetLabel ? undefined : t('settings.startupValue')
  if (secret.value && props.field.configured && props.draft === undefined) return t('settings.configured')
  return undefined
})
</script>

<template>
  <UiBox class="settings-field">
    <UiInput
      :id="'setting-' + field.key"
      :label="label"
      :model-value="value"
      :type="secret ? 'password' : 'text'"
      :placeholder="placeholder"
      :autocomplete="secret ? 'new-password' : 'off'"
      :disabled="disabled"
      @update:model-value="emit('edit', $event)"
    />
    <UiBox class="settings-input-actions">
      <UiButton
        class="secondary-button"
        :hidden="!secret || !field.configured || draft === '' || draft === null"
        :disabled="disabled"
        @click="emit('clear')"
      >
        {{ t('settings.clearSecret') }}
      </UiButton>
      <UiButton
        class="secondary-button"
        :hidden="!field.overridden || draft === null || (secret && !!resetLabel)"
        :disabled="disabled"
        @click="emit('reset')"
      >
        {{ resetLabel ?? t('settings.useStartup') }}
      </UiButton>
    </UiBox>
  </UiBox>
</template>
