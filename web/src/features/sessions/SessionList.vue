<script setup lang="ts">
import { RouterLink } from 'vue-router'

import { t } from '../../shared/i18n'
import { formatRelativeTime, formatTime } from '../../shared/time'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiMoreMenu from '../../shared/ui/UiMoreMenu.vue'
import UiSpinner from '../../shared/ui/UiSpinner.vue'
import { UiBox, UiButton, UiInline, UiList, UiListItem, UiSubheading, UiTime } from '../../shared/ui/elements'
import { sessionTitle } from './presentation'
import type { SessionAction } from './presentation'
import type { SessionSummary } from './sessionApi'
import { useSessionStore } from './sessionStore'

defineProps<{ label: string; entries: SessionSummary[]; now: number; selectedId?: string }>()
const emit = defineEmits<{ select: []; action: [action: SessionAction, entry: SessionSummary] }>()
const store = useSessionStore()

function projectName(id: SessionSummary['projectId']) {
  return id ? store.projectNames.get(id) ?? t('common.unavailable') : t('projects.default')
}
</script>

<template>
  <UiBox class="session-group">
    <UiSubheading>{{ label }}</UiSubheading>
    <UiList class="session-list">
      <UiListItem
        v-for="entry in entries"
        :key="entry.id"
        class="session-entry"
        :class="{ 'session-item--current': selectedId === entry.id }"
      >
        <RouterLink
          class="session-item"
          :to="{ name: 'session', params: { id: entry.id } }"
          :title="sessionTitle(entry) + ' · ' + formatTime(entry.updatedAt)"
          @click="emit('select')"
        >
          <UiSpinner class="session-running" :hidden="!entry.active" />
          <UiIcon name="chat-1-line" :hidden="entry.active" />
          <UiInline class="session-item-text">
            <UiInline class="session-name">{{ sessionTitle(entry) }}</UiInline>
            <UiInline class="session-description">
              <UiInline class="session-project-name" :title="projectName(entry.projectId)">{{ projectName(entry.projectId) }}</UiInline>
              <UiInline aria-hidden="true">·</UiInline>
              <UiTime class="session-updated" :datetime="entry.updatedAt">{{ formatRelativeTime(entry.updatedAt, now) }}</UiTime>
            </UiInline>
          </UiInline>
          <UiIcon name="pushpin-2-line" :hidden="!entry.pinned" />
          <UiInline
            class="unread-dot"
            :hidden="!entry.unread"
            :title="t('sessions.unread')"
            :aria-label="t('sessions.unread')"
          />
        </RouterLink>
        <UiMoreMenu
          :label="t('sessions.actionsFor', { title: sessionTitle(entry) })"
          boundary=".session-group"
        >
          <template #default="{ close }">
            <UiButton class="menu-item" @click="close(true); emit('action', 'pin', entry)">
              <UiIcon name="pushpin-2-line" /> {{ t(entry.pinned ? 'sessions.unpin' : 'sessions.pin') }}
            </UiButton>
            <UiButton class="menu-item" @click="close(true); emit('action', 'rename', entry)">
              <UiIcon name="edit-line" /> {{ t('sessions.rename') }}
            </UiButton>
            <UiButton class="menu-item danger" :disabled="entry.active" @click="close(true); emit('action', 'remove', entry)">
              <UiIcon name="delete-bin-line" /> {{ t('sessions.remove') }}
            </UiButton>
          </template>
        </UiMoreMenu>
      </UiListItem>
    </UiList>
    <UiInline class="empty-list" :hidden="entries.length > 0">{{ t('sessions.none') }}</UiInline>
  </UiBox>
</template>
