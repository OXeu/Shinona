<script setup lang="ts">
import { computed, ref } from 'vue'

import { languageTag, t } from '../../shared/i18n'
import { formatTime } from '../../shared/time'
import UiIcon from '../../shared/ui/UiIcon.vue'
import UiSearchField from '../../shared/ui/UiSearchField.vue'
import { UiBox, UiButton, UiInline, UiList, UiListItem, UiSection, UiTime } from '../../shared/ui/elements'
import type { GitCommit, WorkspaceList } from './workspaceApi'

const props = defineProps<{ history: WorkspaceList<GitCommit> | null }>()
const emit = defineEmits<{ preview: [commit: GitCommit] }>()
const filter = ref('')
const visibleCommits = computed(() => {
  const language = languageTag()
  const query = filter.value.trim().toLocaleLowerCase(language)
  return (props.history?.entries ?? []).filter(({ subject, author, hash }) =>
    [subject, author, hash].some((value) => value.toLocaleLowerCase(language).includes(query)))
})
</script>

<template>
  <UiSection class="workspace-view" :aria-label="t('repository.history')">
    <UiBox class="workspace-search">
      <UiSearchField v-model="filter" :label="t('repository.searchHistory')" />
    </UiBox>
    <UiBox class="workspace-scroll">
      <UiInline v-if="history && !visibleCommits.length" class="workspace-note">
        {{ t(filter.trim() ? 'repository.noMatches' : 'repository.noCommits') }}
      </UiInline>
      <UiInline v-if="history?.truncated" class="workspace-note">{{ t('repository.truncated') }}</UiInline>
      <UiList class="workspace-rows">
        <UiListItem v-for="commit in visibleCommits" :key="commit.hash">
          <UiButton class="workspace-row" :title="commit.subject" @click="emit('preview', commit)">
            <UiIcon name="git-commit-line" />
            <UiBox class="workspace-commit-description">
              <UiInline class="workspace-ellipsis">{{ commit.subject }}</UiInline>
              <UiInline class="workspace-ellipsis workspace-meta">
                {{ commit.short }} · {{ commit.author }} · <UiTime :datetime="commit.date">{{ formatTime(commit.date) }}</UiTime>
              </UiInline>
            </UiBox>
          </UiButton>
        </UiListItem>
      </UiList>
    </UiBox>
  </UiSection>
</template>
