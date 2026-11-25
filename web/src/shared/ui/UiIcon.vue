<script setup lang="ts">
defineProps<{ name: string }>()
</script>

<template>
  <span class="ui-icon" :class="'ri-' + name" aria-hidden="true" />
</template>

<style lang="less" scoped>
.ui-icon { display: inline-block; flex-shrink: 0; line-height: 1; }
</style>
