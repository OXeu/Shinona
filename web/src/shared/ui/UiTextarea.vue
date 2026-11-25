<script setup lang="ts">
defineOptions({ inheritAttrs: false })
defineProps<{ id: string; label: string }>()
const value = defineModel<string>({ required: true })
</script>

<template>
  <div class="field">
    <label :for="id">{{ label }}</label>
    <textarea :id="id" v-model="value" v-bind="$attrs" />
  </div>
</template>
