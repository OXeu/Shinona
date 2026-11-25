<script setup lang="ts">
import { t } from '../../shared/i18n'
import UiDialog from '../../shared/ui/UiDialog.vue'
import { UiBox, UiButton, UiForm, UiSubheading } from '../../shared/ui/elements'

defineProps<{ busy: boolean }>()
const emit = defineEmits<{ confirm: [] }>()
const open = defineModel<boolean>({ required: true })
</script>

<template>
  <UiDialog v-model="open" :label="t('chat.revertMessage')">
    <UiSubheading>{{ t('chat.revertMessage') }}</UiSubheading>
    <UiForm @submit.prevent="emit('confirm')">
      <UiBox class="dialog-message">{{ t('chat.revertConfirm') }}</UiBox>
      <UiBox class="form-actions">
        <UiButton class="secondary-button" :disabled="busy" @click="open = false">{{ t('common.cancel') }}</UiButton>
        <UiButton type="submit" :disabled="busy">{{ t('chat.revertAction') }}</UiButton>
      </UiBox>
    </UiForm>
  </UiDialog>
</template>
