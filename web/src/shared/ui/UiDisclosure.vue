<template>
  <details class="ui-disclosure">
    <summary class="ui-disclosure-trigger">
      <slot name="summary" />
    </summary>
    <slot />
  </details>
</template>

<style lang="less" scoped>
.ui-disclosure-trigger { list-style: none; cursor: pointer; }
.ui-disclosure-trigger::-webkit-details-marker { display: none; }
</style>
