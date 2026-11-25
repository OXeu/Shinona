<template>
  <span class="ui-spinner" aria-hidden="true" />
</template>

<style lang="less" scoped>
.ui-spinner {
  display: inline-block;
  flex: none;
  inline-size: 1em;
  block-size: 1em;
  border: 2px solid color-mix(in srgb, currentColor 25%, transparent);
  border-inline-start-color: currentColor;
  border-radius: 50%;
  animation: spinner-rotate 0.8s linear infinite;
}

@keyframes spinner-rotate {
  to { transform: rotate(360deg); }
}

@media (prefers-reduced-motion: reduce) {
  .ui-spinner { animation: none; }
}
</style>
