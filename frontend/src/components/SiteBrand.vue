<template>
  <span class="site-brand" :aria-label="compact ? brand.name : undefined">
    <img class="site-brand-mark" src="/logo.svg" alt="" width="32" height="32" />
    <span v-if="!compact">{{ brand.name || 'Best AI' }}</span>
  </span>
</template>

<script setup lang="ts">
import { siteBrand } from '../theme'

withDefaults(defineProps<{ compact?: boolean }>(), { compact: false })

const brand = siteBrand
</script>

<style scoped>
/* Match sub2api's HomeView .home-brand and its system sans font stack. */
.site-brand {
  display: inline-flex;
  flex-shrink: 0;
  align-items: center;
  gap: 9px;
  color: #173b32;
  font-family: system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto,
    'Helvetica Neue', Arial, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif;
  font-size: 23px;
  font-weight: 700;
  line-height: 1.5;
  text-decoration: none;
  white-space: nowrap;
}

.site-brand-mark {
  display: block;
  flex-shrink: 0;
  width: 32px;
  height: 32px;
  border-radius: 8px;
  object-fit: contain;
}

:global(html.dark .site-brand) { color: #edf4e5; }

@media (max-width: 820px) {
  .site-brand { font-size: 20px; }
}
</style>
