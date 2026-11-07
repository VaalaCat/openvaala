<script setup lang="ts">
import { nextTick, onMounted, ref, watch } from "vue"
import type { OutlineHeading } from "../types"

const props = defineProps<{ headings: OutlineHeading[]; activeId: string; variant: "aside" | "menu" }>()
const emit = defineEmits<{ (e: "navigate"): void }>()

const listEl = ref<HTMLElement>()

// 当前标题指示条沿左侧细线滑动：位置用 translateY，长度用 scaleY，只动 transform。
const indicatorTop = ref(0)
const indicatorScale = ref(0)
const isIndicatorVisible = ref(false)
const isIndicatorGliding = ref(false)

// placeIndicator 把指示条对齐到当前标题条目；首次出现时不滑动。
function placeIndicator() {
  const activeLink = props.activeId
    ? listEl.value?.querySelector<HTMLElement>(`a[href="#${CSS.escape(props.activeId)}"]`)
    : undefined
  if (!activeLink) {
    isIndicatorVisible.value = false
    return
  }
  isIndicatorGliding.value = isIndicatorVisible.value
  indicatorTop.value = activeLink.offsetTop
  indicatorScale.value = activeLink.offsetHeight
  isIndicatorVisible.value = true
}

watch(() => [props.activeId, props.headings], () => nextTick(placeIndicator))
onMounted(placeIndicator)
</script>

<template>
  <div ref="listEl" class="outline-list" :class="`outline-list--${variant}`">
    <span
      v-if="variant === 'aside'"
      class="outline-indicator"
      :class="{ 'is-visible': isIndicatorVisible, 'is-gliding': isIndicatorGliding }"
      :style="{ transform: `translateY(${indicatorTop}px) scaleY(${indicatorScale})` }"
      aria-hidden="true"
    />
    <ul>
      <li v-for="heading in headings" :key="heading.id" :class="`outline-item--h${heading.level}`">
        <a
          class="outline-link"
          :href="`#${heading.id}`"
          :aria-current="heading.id === activeId ? 'location' : undefined"
          @click="emit('navigate')"
        >
          {{ heading.text }}
        </a>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.outline-list {
  position: relative;
}

.outline-list ul {
  margin: 0;
  padding: 0;
  list-style: none;
}

.outline-list--aside {
  border-left: 1px solid var(--rule);
}

.outline-indicator {
  position: absolute;
  top: 0;
  left: -1px;
  width: 2px;
  height: 1px;
  background: var(--signal);
  opacity: 0;
  transform-origin: top;
  will-change: transform;
}

.outline-indicator.is-visible {
  opacity: 1;
}

.outline-indicator.is-gliding {
  transition: transform 260ms cubic-bezier(0.2, 0.8, 0.2, 1), opacity 160ms ease-out;
}

.outline-link {
  display: block;
  padding: 5px 0 5px 14px;
  color: var(--ink-muted);
  font-size: 0.8125rem;
  line-height: 1.45;
  text-decoration: none;
}

.outline-item--h3 .outline-link {
  padding-left: 26px;
}

.outline-link:hover,
.outline-link[aria-current="location"] {
  color: var(--ink);
}

.outline-list--menu .outline-link {
  padding: 7px 10px;
  border-radius: 8px;
  font-size: 0.875rem;
}

.outline-list--menu .outline-item--h3 .outline-link {
  padding-left: 22px;
}

.outline-list--menu .outline-link[aria-current="location"] {
  background: var(--paper-sunken);
}
</style>
