<script setup lang="ts">
import { t } from '../../shared/i18n'
import { UiBox, UiButton, UiInline } from '../../shared/ui/elements'
import UiIcon from '../../shared/ui/UiIcon.vue'

defineProps<{ ready: boolean; saved: boolean; saving: boolean }>()
const emit = defineEmits<{ save: []; reset: [] }>()
</script>

<template>
  <UiBox class="config-default-actions">
    <UiBox class="config-default-header">
      <UiButton
        class="config-default-badge"
        :data-saved="saved"
        :disabled="!ready || saving || saved"
        :title="t('composer.setDefaultHint')"
        @click="emit('save')"
      >
        <UiIcon :name="saved ? 'checkbox-circle-fill' : 'pushpin-line'" />
        {{ t(saved ? 'composer.defaultSaved' : 'composer.setDefault') }}
      </UiButton>
    </UiBox>
    <slot />
    <UiButton
      class="config-default-reset"
      :disabled="!ready || saving"
      @click="emit('reset')"
    >
      <UiInline>{{ t('composer.resetDefaults') }}</UiInline>
      <UiIcon name="arrow-go-back-line" />
    </UiButton>
  </UiBox>
</template>

<style lang="less" scoped>
.config-default-actions { display: grid; gap: 2px; }
.config-default-header { display: flex; justify-content: flex-end; padding: 1px 4px 4px; }
.config-default-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  min-height: 24px;
  padding: 2px 8px;
  border: 1px solid color-mix(in srgb, var(--link) 35%, var(--border));
  border-radius: var(--radius-full);
  background: color-mix(in srgb, var(--link) 10%, var(--elevated));
  color: var(--link);
  font-size: 0.6875rem;
  font-weight: 600;
}
.config-default-badge:hover:not(:disabled) { border-color: var(--link); }
.config-default-badge[data-saved='true'] {
  cursor: default;
  opacity: 1;
  border-color: color-mix(in srgb, var(--success) 40%, var(--border));
  background: color-mix(in srgb, var(--success) 10%, var(--elevated));
  color: var(--success);
}
.config-default-reset {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
  min-height: 34px;
  margin-top: 4px;
  padding: 5px 7px;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--muted);
  font-size: 0.75rem;
  font-weight: 500;
  text-align: start;
}
.config-default-reset:hover:not(:disabled) { background: var(--sidebar-bg); }
@media (max-width: 767px) {
  .config-default-actions { gap: 8px; }
  .config-default-badge, .config-default-reset { min-height: 44px; }
}
</style>
