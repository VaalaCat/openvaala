<script setup lang="ts">
import { computed } from "vue"
import { type AppearanceMode, useAppearanceMode, useSiteText } from "../site"

const text = useSiteText()
const { mode, setMode } = useAppearanceMode()

// 依次切换：跟随系统 → 浅色 → 深色 → 跟随系统。
const nextModeByMode: Record<AppearanceMode, AppearanceMode> = { auto: "light", light: "dark", dark: "auto" }

const label = computed(() => `${text.value.themeLabel[mode.value]} · ${text.value.themeNext}`)
</script>

<template>
  <button
    class="theme-toggle"
    type="button"
    :data-mode="mode"
    :aria-label="label"
    :title="label"
    @click="setMode(nextModeByMode[mode])"
  >
    <svg viewBox="0 0 20 20" aria-hidden="true">
      <defs>
        <clipPath id="theme-toggle-disc">
          <circle cx="10" cy="10" r="6.25" />
        </clipPath>
      </defs>
      <g clip-path="url(#theme-toggle-disc)">
        <rect class="theme-toggle-fill" x="0" y="0" width="20" height="20" />
      </g>
      <circle class="theme-toggle-ring" cx="10" cy="10" r="6.25" />
    </svg>
  </button>
</template>

<style scoped>
.theme-toggle {
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

.theme-toggle:hover {
  background: var(--paper-sunken);
  color: var(--ink);
}

svg {
  width: 20px;
  height: 20px;
}

.theme-toggle-ring {
  fill: none;
  stroke: currentColor;
  stroke-width: 1.5;
}

/* 填充块从左侧推入圆盘：浅色为空心 ○，跟随系统为半满 ◐，深色为实心 ●。 */
.theme-toggle-fill {
  fill: currentColor;
  transform: translateX(-10px);
  transition: transform 240ms cubic-bezier(0.2, 0.8, 0.2, 1);
}

[data-mode="light"] .theme-toggle-fill {
  transform: translateX(-20px);
}

[data-mode="dark"] .theme-toggle-fill {
  transform: translateX(0);
}
</style>
