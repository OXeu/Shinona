<script setup lang="ts">
import { locale, localeOptions, setLocale, t } from '../i18n'
import UiChoiceDropdown from './UiChoiceDropdown.vue'

defineProps<{ boundary?: string; textTrigger?: boolean }>()
</script>

<template>
  <UiChoiceDropdown
    :label="t('common.language')"
    :model-value="locale"
    :options="localeOptions"
    :boundary="boundary"
    :text-trigger="textTrigger"
    @update:model-value="setLocale"
  />
</template>
