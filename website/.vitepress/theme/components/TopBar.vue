<script setup lang="ts">
import { computed, onMounted, ref } from "vue"
import { useData, withBase } from "vitepress"
import LogoMark from "./LogoMark.vue"
import ThemeToggle from "./ThemeToggle.vue"
import { useAlternateLocaleLink, useSiteText } from "../site"
import type { ThemeConfig } from "../types"

defineProps<{ isSidebarOpen: boolean }>()
const emit = defineEmits<{ (e: "open-search"): void; (e: "toggle-sidebar"): void }>()

const { localeIndex, theme } = useData<ThemeConfig>()
const text = useSiteText()
const alternateLocaleLink = useAlternateLocaleLink()

const isZh = computed(() => localeIndex.value === "zh")
const homeLink = computed(() => withBase(isZh.value ? "/zh/" : "/"))

// 服务端渲染统一显示 Ctrl，挂载后在 Apple 设备上换成 ⌘。
const modifierKey = ref("Ctrl")
onMounted(() => {
  if (/Mac|iPhone|iPad/.test(navigator.platform)) modifierKey.value = "⌘"
})
</script>

<template>
  <header class="top-bar">
    <div class="top-bar-inner">
      <button
        class="icon-button menu-button"
        type="button"
        :aria-label="isSidebarOpen ? text.closeMenu : text.menu"
        :aria-expanded="isSidebarOpen"
        aria-controls="site-sidebar"
        @click="emit('toggle-sidebar')"
      >
        <svg viewBox="0 0 20 20" aria-hidden="true">
          <path d="M3 6h14M3 10h14M3 14h9" />
        </svg>
      </button>

      <a class="brand" :href="homeLink">
        <LogoMark />
        <span class="brand-wordmark">OpenVaala</span>
      </a>

      <div class="top-bar-actions">
        <button class="search-trigger" type="button" :aria-label="text.searchLabel" @click="emit('open-search')">
          <svg class="search-trigger-icon" viewBox="0 0 20 20" aria-hidden="true">
            <circle cx="8.5" cy="8.5" r="5.25" />
            <path d="m12.5 12.5 4 4" />
          </svg>
          <span class="search-trigger-text">{{ text.search }}</span>
          <kbd class="search-trigger-keys">{{ modifierKey }} K</kbd>
        </button>

        <nav class="locale-switch" aria-label="Language">
          <component
            :is="isZh ? 'a' : 'span'"
            :href="isZh ? withBase(alternateLocaleLink) : undefined"
            :aria-current="isZh ? undefined : 'true'"
            lang="en"
            hreflang="en"
          >
            EN
          </component>
          <span class="locale-switch-rule" aria-hidden="true" />
          <component
            :is="isZh ? 'span' : 'a'"
            :href="isZh ? undefined : withBase(alternateLocaleLink)"
            :aria-current="isZh ? 'true' : undefined"
            lang="zh-CN"
            hreflang="zh-CN"
          >
            中
          </component>
        </nav>

        <ThemeToggle />

        <a class="icon-button github-link" :href="theme.repository" :aria-label="text.github" target="_blank" rel="noreferrer">
          <svg viewBox="0 0 16 16" aria-hidden="true">
            <path
              d="M8 0C3.58 0 0 3.58 0 8a8 8 0 0 0 5.47 7.59c.4.07.55-.17.55-.38v-1.33c-2.23.48-2.7-1.07-2.7-1.07-.36-.92-.89-1.17-.89-1.17-.73-.5.05-.49.05-.49.8.06 1.23.83 1.23.83.72 1.22 1.87.87 2.33.66.07-.52.28-.87.5-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48v2.2c0 .21.15.46.55.38A8 8 0 0 0 16 8c0-4.42-3.58-8-8-8Z"
            />
          </svg>
        </a>
      </div>
    </div>
  </header>
</template>

<style scoped>
.top-bar {
  position: sticky;
  top: 0;
  z-index: 30;
  height: var(--top-bar-height);
  border-bottom: 1px solid var(--rule);
  background: color-mix(in srgb, var(--paper) 86%, transparent);
  backdrop-filter: saturate(1.4) blur(10px);
  -webkit-backdrop-filter: saturate(1.4) blur(10px);
}

.top-bar-inner {
  display: flex;
  align-items: center;
  gap: 12px;
  max-width: var(--site-max-width);
  height: 100%;
  margin: 0 auto;
  padding: 0 var(--site-gutter);
}

.brand {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  margin-right: auto;
  border-radius: 8px;
  color: var(--ink);
  text-decoration: none;
}

.brand-wordmark {
  font-family: var(--font-display);
  font-size: 1.25rem;
  font-weight: 500;
  letter-spacing: -0.01em;
}

.top-bar-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.icon-button {
  display: inline-grid;
  place-items: center;
  width: 36px;
  height: 36px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--ink-soft);
  cursor: pointer;
}

.icon-button:hover {
  background: var(--paper-sunken);
  color: var(--ink);
}

.icon-button svg {
  width: 18px;
  height: 18px;
}

.menu-button svg {
  fill: none;
  stroke: currentColor;
  stroke-width: 1.6;
  stroke-linecap: round;
}

.github-link svg {
  fill: currentColor;
}

.menu-button {
  display: none;
  margin-left: -8px;
}

.search-trigger {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  width: 232px;
  height: 36px;
  margin-right: 10px;
  padding: 0 8px 0 12px;
  border: 1px solid var(--rule);
  border-radius: 10px;
  background: var(--paper-raised);
  color: var(--ink-muted);
  font: inherit;
  font-size: 0.875rem;
  cursor: pointer;
}

.search-trigger:hover {
  border-color: var(--rule-strong);
  color: var(--ink-soft);
}

.search-trigger-icon {
  flex: none;
  width: 16px;
  height: 16px;
  fill: none;
  stroke: currentColor;
  stroke-width: 1.6;
  stroke-linecap: round;
}

.search-trigger-text {
  flex: 1;
  text-align: left;
}

.search-trigger-keys {
  padding: 2px 6px;
  border: 1px solid var(--rule);
  border-radius: 6px;
  background: var(--paper);
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  letter-spacing: 0.04em;
}

.locale-switch {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 36px;
  padding: 0 8px;
  font-family: var(--font-code);
  font-size: 0.75rem;
  letter-spacing: 0.06em;
}

.locale-switch > a,
.locale-switch > span[aria-current] {
  padding: 2px 0;
  border-radius: 4px;
  text-decoration: none;
}

.locale-switch > span[aria-current] {
  color: var(--ink);
  font-weight: 600;
}

.locale-switch > a {
  color: var(--ink-muted);
}

.locale-switch > a:hover {
  color: var(--ink);
}

.locale-switch-rule {
  width: 1px;
  height: 12px;
  background: var(--rule-strong);
}

@media (max-width: 959px) {
  .menu-button {
    display: inline-grid;
  }
}

@media (max-width: 719px) {
  .top-bar-inner {
    gap: 4px;
  }

  .search-trigger {
    width: 36px;
    margin-right: 0;
    padding: 0;
    justify-content: center;
    border-color: transparent;
    background: transparent;
    color: var(--ink-soft);
  }

  .search-trigger-text,
  .search-trigger-keys,
  .github-link {
    display: none;
  }

  .search-trigger-icon {
    width: 18px;
    height: 18px;
  }

  .locale-switch {
    padding: 0 4px;
  }
}
</style>
