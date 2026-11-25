<script setup lang="ts">
import { computed } from 'vue'

import { t } from '../../shared/i18n'
import UiDropdown from '../../shared/ui/UiDropdown.vue'
import UiIcon from '../../shared/ui/UiIcon.vue'
import { UiBox, UiButton, UiInline } from '../../shared/ui/elements'

const props = defineProps<{ servers: string[]; disabled: boolean; mcpDisabled: boolean }>()
const selected = defineModel<string[]>({ required: true })
const arrowJs = defineModel<boolean>('arrowJs', { required: true })
const groups = computed(() => [
  {
    label: t('composer.pluginGroup'),
    options: [{ label: 'ArrowJS', enabled: arrowJs.value, disabled: false, toggle: () => { arrowJs.value = !arrowJs.value } }],
  },
  {
    label: 'MCP',
    options: props.servers.map(name => ({
      label: name,
      enabled: selected.value.includes(name),
      disabled: props.mcpDisabled,
      toggle: () => toggleMcp(name),
    })),
  },
])
const count = computed(() => groups.value.flatMap(group => group.options).filter(option => option.enabled && !option.disabled).length)

function toggleMcp(name: string) {
  selected.value = selected.value.includes(name)
    ? selected.value.filter(server => server !== name)
    : [...selected.value, name]
}
</script>

<template>
  <UiDropdown
    :label="count ? t('composer.pluginsSelected', { count }) : t('composer.plugins')"
    :disabled="disabled"
    boundary=".composer-toolbar"
    align="start"
    side="top"
    floating
    scroll
  >
    <template #trigger>
      <UiBox class="composer-control" :data-selected="count > 0">
        <UiIcon name="plug-line" />
      </UiBox>
    </template>
    <UiBox
      v-for="group in groups"
      :key="group.label"
      role="group"
      :aria-label="group.label"
    >
      <UiInline class="plugin-description">{{ group.label }}</UiInline>
      <UiInline class="plugin-description" :hidden="!!group.options.length">{{ t('composer.noMcpServers') }}</UiInline>
      <UiButton
        v-for="option in group.options"
        :key="option.label"
        class="menu-item plugin-option"
        role="switch"
        :aria-checked="option.enabled"
        :disabled="disabled || option.disabled"
        @click="option.toggle()"
      >
        <UiInline>{{ option.label }}</UiInline>
        <UiInline class="plugin-switch" aria-hidden="true">
          <UiInline class="plugin-switch-thumb">
            <UiIcon :name="option.enabled ? 'check-line' : 'subtract-line'" />
          </UiInline>
        </UiInline>
      </UiButton>
    </UiBox>
  </UiDropdown>
</template>

<style lang="less" scoped>
.composer-control {
  display: flex;
  align-items: center;
  min-height: 32px;
  padding-inline: 6px;
  border-radius: var(--radius-sm);
  color: var(--muted);
}
.composer-control:hover, .composer-control[data-selected='true'] {
  background: var(--sidebar-bg);
  color: var(--foreground);
}
.composer-control .ui-icon { font-size: 1rem; }
.plugin-description { display: block; padding: 5px 8px; color: var(--muted); font-size: 0.75rem; }
.plugin-option {
  min-width: min(13rem, 100%);
  justify-content: space-between;
  gap: 12px;
  white-space: normal;
}
.plugin-switch {
  display: flex;
  align-items: center;
  flex: 0 0 2.75rem;
  height: 1.5rem;
  padding-inline: 2px;
  border: 1px solid var(--border);
  border-radius: var(--radius-full);
  background: var(--sidebar-bg);
}
.plugin-switch-thumb {
  display: grid;
  place-items: center;
  width: 1.125rem;
  height: 1.125rem;
  border-radius: var(--radius-full);
  background: var(--background);
  color: var(--muted);
  font-size: 0.75rem;
  box-shadow: 0 1px 2px var(--border);
}
.plugin-option[aria-checked='true'] .plugin-switch {
  justify-content: flex-end;
  border-color: var(--primary);
  background: var(--primary);
}
.plugin-option[aria-checked='true'] .plugin-switch-thumb { color: var(--accent); }
</style>
