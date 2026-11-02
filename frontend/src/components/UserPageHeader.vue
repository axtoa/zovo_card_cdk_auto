<template>
  <!-- 用户页统一页头：顶栏（品牌 + 语言/明暗）+ 居中标题区（标签、标题、说明、信任徽章）。
       结构各皮肤共用，外观由 styles/skins.css 按 data-skin 决定。 -->
  <header class="u-topbar">
    <router-link to="/" class="u-brand" :aria-label="t('common.backHome')">
      <SiteBrand />
    </router-link>
    <div class="u-tools">
      <LanguageToggle />
      <ThemeToggle />
    </div>
  </header>

  <section class="u-hero" :class="{ 'u-hero--compact': compact }">
    <span v-if="eyebrow" class="u-eyebrow"><i aria-hidden="true" />{{ eyebrow }}</span>
    <h1 class="u-title">{{ title }}</h1>
    <p v-if="subtitle" class="u-sub">{{ subtitle }}</p>
    <ul v-if="badges" class="u-badges">
      <li v-for="b in badgeList" :key="b">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" /><path d="m9 12 2 2 4-4" /></svg>
        {{ b }}
      </li>
    </ul>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import LanguageToggle from './LanguageToggle.vue'
import ThemeToggle from './ThemeToggle.vue'
import SiteBrand from './SiteBrand.vue'

withDefaults(defineProps<{
  title: string
  subtitle?: string
  eyebrow?: string
  badges?: boolean
  compact?: boolean
}>(), { subtitle: '', eyebrow: '', badges: false, compact: false })

const { t } = useI18n({ useScope: 'global' })
const badgeList = computed(() => [t('hero.badgeOfficial'), t('hero.badgeSelf'), t('hero.badgeTrack')])
</script>
