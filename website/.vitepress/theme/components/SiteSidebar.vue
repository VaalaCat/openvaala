<script setup lang="ts">
import { nextTick, onMounted, onUnmounted, ref, watch } from "vue"
import { useData, withBase } from "vitepress"
import { usePagePosition, useSiteText } from "../site"
import type { ThemeConfig } from "../types"

const props = defineProps<{ isOpen: boolean; isDrawerOnly: boolean }>()
const emit = defineEmits<{ (e: "close"): void }>()

const { theme } = useData<ThemeConfig>()
const text = useSiteText()
const pagePosition = usePagePosition()

const navEl = ref<HTMLElement>()
const markerEl = ref<HTMLElement>()
const markerBarEl = ref<HTMLElement>()
const closeButtonEl = ref<HTMLButtonElement>()

// 当前页标记是一块跨导航移动的元素：挂载后接管服务端渲染的静态高亮，换页时滑到新条目。
const isMarkerReady = ref(false)
const isMarkerVisible = ref(false)
const isMarkerGliding = ref(false)
const markerTop = ref(0)
const markerHeight = ref(0)

// placeMarker 把标记移到当前页条目；glide 为 true 时滑动过去，并让光标条在途中拉长再回弹。
function placeMarker(glide: boolean) {
  const activeLink = navEl.value?.querySelector<HTMLElement>('a[aria-current="page"]')
  if (!activeLink) {
    isMarkerVisible.value = false
    return
  }
  const hasMoved = markerTop.value !== activeLink.offsetTop
  isMarkerGliding.value = glide && isMarkerVisible.value
  markerTop.value = activeLink.offsetTop
  markerHeight.value = activeLink.offsetHeight
  isMarkerVisible.value = true
  if (isMarkerGliding.value && hasMoved) {
    markerBarEl.value?.animate(
      [{ transform: "scaleY(1)" }, { transform: "scaleY(1.9)", offset: 0.4 }, { transform: "scaleY(1)" }],
      { duration: 300, easing: "cubic-bezier(0.2, 0.8, 0.2, 1)" },
    )
  }
}

watch(
  () => pagePosition.value.currentLink,
  () => nextTick(() => placeMarker(true)),
)

// 侧栏组名或语言变化会改变条目高度，重新定位但不滑动。
watch(
  () => theme.value.sidebar,
  () => nextTick(() => placeMarker(false)),
)

let resizeObserver: ResizeObserver | undefined
onMounted(() => {
  placeMarker(false)
  isMarkerReady.value = true
  resizeObserver = new ResizeObserver(() => placeMarker(false))
  if (navEl.value) resizeObserver.observe(navEl.value)
})
onUnmounted(() => resizeObserver?.disconnect())

watch(
  () => props.isOpen,
  (isOpen) => {
    if (isOpen) nextTick(() => closeButtonEl.value?.focus())
  },
)

function handleKeydown(event: KeyboardEvent) {
  if (event.key === "Escape" && props.isOpen) emit("close")
}
</script>

<template>
  <div
    class="sidebar-backdrop"
    :class="{ 'is-open': isOpen, 'is-drawer-only': isDrawerOnly }"
    aria-hidden="true"
    @click="emit('close')"
  />
  <aside
    id="site-sidebar"
    class="site-sidebar"
    :class="{ 'is-open': isOpen, 'is-drawer-only': isDrawerOnly }"
    :aria-label="text.docsNavigation"
    @keydown="handleKeydown"
  >
    <div class="sidebar-drawer-head">
      <button ref="closeButtonEl" class="sidebar-close" type="button" :aria-label="text.closeMenu" @click="emit('close')">
        <svg viewBox="0 0 20 20" aria-hidden="true"><path d="m5 5 10 10M15 5 5 15" /></svg>
      </button>
    </div>

    <nav
      ref="navEl"
      class="sidebar-nav"
      :class="{ 'has-marker': isMarkerReady }"
      :aria-label="text.docsNavigation"
    >
      <div
        ref="markerEl"
        class="sidebar-marker"
        :class="{ 'is-visible': isMarkerVisible, 'is-gliding': isMarkerGliding }"
        :style="{ transform: `translateY(${markerTop}px)`, height: `${markerHeight}px` }"
        aria-hidden="true"
      >
        <span ref="markerBarEl" class="sidebar-marker-bar" />
      </div>

      <section v-for="group in theme.sidebar" :key="group.text" class="sidebar-group">
        <h2 class="sidebar-group-label">{{ group.text }}</h2>
        <ul>
          <li v-for="item in group.items" :key="item.link">
            <a
              class="sidebar-link"
              :href="withBase(item.link)"
              :aria-current="item.link === pagePosition.currentLink ? 'page' : undefined"
            >
              {{ item.text }}
            </a>
          </li>
        </ul>
      </section>
    </nav>
  </aside>
</template>

<style scoped>
.site-sidebar {
  position: sticky;
  top: var(--top-bar-height);
  align-self: start;
  height: calc(100dvh - var(--top-bar-height));
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 40px 12px 48px 0;
  scrollbar-width: thin;
  scrollbar-color: transparent transparent;
}

/* 滚动条只在指针停在侧栏上时显示。 */
.site-sidebar:hover {
  scrollbar-color: var(--rule-strong) transparent;
}

.site-sidebar.is-drawer-only {
  display: none;
}

.sidebar-drawer-head,
.sidebar-backdrop {
  display: none;
}

.sidebar-nav {
  position: relative;
}

.sidebar-group + .sidebar-group {
  margin-top: 28px;
}

.sidebar-group-label {
  margin: 0 0 8px;
  padding: 0 12px;
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  font-weight: 500;
  letter-spacing: 0.09em;
  line-height: 1.6;
  text-transform: uppercase;
}

.sidebar-group ul {
  margin: 0;
  padding: 0;
  list-style: none;
}

.sidebar-link {
  position: relative;
  z-index: 1;
  display: block;
  padding: 6px 12px 6px 18px;
  border-radius: 8px;
  color: var(--ink-soft);
  font-size: 0.9375rem;
  line-height: 1.5;
  text-decoration: none;
}

.sidebar-link:hover {
  color: var(--ink);
  background: color-mix(in srgb, var(--paper-sunken) 60%, transparent);
}

.sidebar-link[aria-current="page"] {
  color: var(--ink);
  font-weight: 500;
}

/* 服务端渲染与脚本接管前的静态高亮，位置与移动标记一致，接管时没有跳动。 */
.sidebar-nav:not(.has-marker) .sidebar-link[aria-current="page"] {
  background: var(--paper-sunken);
  box-shadow: inset 3px 0 0 var(--signal);
}

.sidebar-nav.has-marker .sidebar-link[aria-current="page"]:hover {
  background: transparent;
}

.sidebar-marker {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  border-radius: 8px;
  background: var(--paper-sunken);
  opacity: 0;
  pointer-events: none;
  will-change: transform;
}

.sidebar-marker.is-visible {
  opacity: 1;
}

.sidebar-marker.is-gliding {
  transition: transform 300ms cubic-bezier(0.2, 0.8, 0.2, 1);
}

/* ▍ 光标条：信号蓝，表示当前页。 */
.sidebar-marker-bar {
  position: absolute;
  top: 9px;
  bottom: 9px;
  left: 6px;
  width: 3px;
  background: var(--signal);
  transform-origin: center;
}

@media (max-width: 959px) {
  .site-sidebar,
  .site-sidebar.is-drawer-only {
    position: fixed;
    top: 0;
    bottom: 0;
    left: 0;
    z-index: 50;
    display: block;
    width: min(320px, 86vw);
    height: auto;
    padding: 0 16px 40px;
    border-right: 1px solid var(--rule);
    background: var(--paper);
    transform: translateX(-100%);
    visibility: hidden;
    transition: transform 260ms cubic-bezier(0.2, 0.8, 0.2, 1), visibility 0s linear 260ms;
  }

  .site-sidebar.is-open {
    transform: translateX(0);
    visibility: visible;
    transition: transform 260ms cubic-bezier(0.2, 0.8, 0.2, 1);
  }

  .sidebar-drawer-head {
    position: sticky;
    top: 0;
    z-index: 2;
    display: flex;
    justify-content: flex-end;
    height: var(--top-bar-height);
    align-items: center;
    margin-bottom: 8px;
    background: var(--paper);
  }

  .sidebar-close {
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

  .sidebar-close svg {
    width: 18px;
    height: 18px;
    fill: none;
    stroke: currentColor;
    stroke-width: 1.6;
    stroke-linecap: round;
  }

  .sidebar-backdrop,
  .sidebar-backdrop.is-drawer-only {
    position: fixed;
    inset: 0;
    z-index: 45;
    display: block;
    background: color-mix(in srgb, var(--ink) 28%, transparent);
    opacity: 0;
    pointer-events: none;
    transition: opacity 220ms ease-out;
  }

  .sidebar-backdrop.is-open {
    opacity: 1;
    pointer-events: auto;
  }
}
</style>
