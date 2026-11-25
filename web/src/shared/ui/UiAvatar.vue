<script setup lang="ts">
import { ref, watch } from 'vue'

import { UiImage, UiInline } from './elements'

const props = defineProps<{ src?: string; initials: string }>()
const loaded = ref(false)

watch(() => props.src, () => { loaded.value = false })
</script>

<template>
  <UiInline class="ui-avatar" aria-hidden="true">
    <UiInline class="avatar-initials" :hidden="loaded">{{ initials }}</UiInline>
    <UiImage
      :key="src"
      class="avatar-image"
      :src="src || undefined"
      :hidden="!loaded"
      alt=""
      referrerpolicy="no-referrer"
      @load="loaded = true"
      @error="loaded = false"
    />
  </UiInline>
</template>

<style lang="less" scoped>
.ui-avatar {
  display: grid;
  flex: 0 0 auto;
  width: 25px;
  height: 25px;
  overflow: hidden;
  place-items: center;
  border-radius: var(--radius-full);
  background: var(--selection-bg);
  color: var(--foreground-soft);
  font-size: 0.66rem;
  font-weight: 750;
}

.avatar-image {
  width: 100%;
  height: 100%;
  object-fit: cover;
}
</style>
