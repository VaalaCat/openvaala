<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue"
import { onContentUpdated, useData, useRoute, withBase } from "vitepress"
import PageOutline from "./PageOutline.vue"
import Scene from "../scenes/Scene.vue"
import { getPageSceneName } from "../scenes/scene-names"
import { usePagePosition, useSiteText } from "../site"
import type { OutlineHeading } from "../types"

const { page } = useData()
const route = useRoute()
const text = useSiteText()
const pageSceneName = computed(() => getPageSceneName(page.value.relativePath))
const pagePosition = usePagePosition()

const contentEl = ref<HTMLElement>()

// 本页标题列表和滚动位置对应的当前标题由 DocPage 持有，桌面侧栏和窄屏下拉共用。
const headings = ref<OutlineHeading[]>([])
const activeHeadingId = ref("")
const isOutlineMenuOpen = ref(false)

// collectHeadings 从正文读取 H2、H3，去掉标题里的永久链接符号。
function collectHeadings() {
  const elements = contentEl.value?.querySelectorAll<HTMLElement>("h2[id], h3[id]") ?? []
  headings.value = Array.from(elements).map((element) => {
    const clone = element.cloneNode(true) as HTMLElement
    clone.querySelectorAll(".header-anchor").forEach((anchor) => anchor.remove())
    return { id: element.id, text: clone.textContent?.trim() ?? "", level: element.tagName === "H3" ? 3 : 2 }
  })
  updateActiveHeading()
}

// updateActiveHeading 选出最后一个越过顶栏下方阅读线的标题；滚到页底时选最后一个。
function updateActiveHeading() {
  const list = headings.value
  if (list.length === 0) {
    activeHeadingId.value = ""
    return
  }
  const isAtBottom = window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2
  if (isAtBottom) {
    activeHeadingId.value = list[list.length - 1].id
    return
  }
  const readingLine = 120
  let activeId = ""
  for (const heading of list) {
    const element = document.getElementById(heading.id)
    if (!element || element.getBoundingClientRect().top > readingLine) break
    activeId = heading.id
  }
  activeHeadingId.value = activeId
}

let scrollFrame = 0
function handleScroll() {
  if (scrollFrame) return
  scrollFrame = requestAnimationFrame(() => {
    scrollFrame = 0
    updateActiveHeading()
  })
}

onContentUpdated(collectHeadings)
onMounted(() => {
  collectHeadings()
  window.addEventListener("scroll", handleScroll, { passive: true })
})
onUnmounted(() => {
  window.removeEventListener("scroll", handleScroll)
  cancelAnimationFrame(scrollFrame)
})

watch(
  () => route.path,
  () => {
    isOutlineMenuOpen.value = false
  },
)

function handleOutlineMenuKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") isOutlineMenuOpen.value = false
}
</script>

<template>
  <div class="doc-page">
    <div v-if="headings.length > 0" class="outline-bar" @keydown="handleOutlineMenuKeydown">
      <button
        class="outline-bar-toggle"
        type="button"
        :aria-expanded="isOutlineMenuOpen"
        aria-controls="outline-menu"
        @click="isOutlineMenuOpen = !isOutlineMenuOpen"
      >
        <span>{{ text.onThisPage }}</span>
        <svg viewBox="0 0 16 16" aria-hidden="true"><path d="m4 6 4 4 4-4" /></svg>
      </button>
      <div v-show="isOutlineMenuOpen" id="outline-menu" class="outline-bar-menu">
        <PageOutline :headings="headings" :active-id="activeHeadingId" variant="menu" @navigate="isOutlineMenuOpen = false" />
      </div>
    </div>

    <div class="doc-columns">
      <article class="doc-article">
        <Scene v-if="pageSceneName" :key="pageSceneName" class="doc-band" :name="pageSceneName" variant="band" />
        <p v-if="pagePosition.group" class="eyebrow">{{ pagePosition.group.text }}</p>
        <div :key="route.path" ref="contentEl" class="prose doc-content">
          <Content />
        </div>

        <nav
          v-if="pagePosition.previous || pagePosition.next"
          class="doc-pager"
          :aria-label="`${text.previous} / ${text.next}`"
        >
          <a v-if="pagePosition.previous" class="doc-pager-link" :href="withBase(pagePosition.previous.link)" rel="prev">
            <span class="doc-pager-label">{{ text.previous }}</span>
            <span class="doc-pager-title">{{ pagePosition.previous.text }}</span>
          </a>
          <a
            v-if="pagePosition.next"
            class="doc-pager-link doc-pager-link--next"
            :href="withBase(pagePosition.next.link)"
            rel="next"
          >
            <span class="doc-pager-label">{{ text.next }}</span>
            <span class="doc-pager-title">{{ pagePosition.next.text }}</span>
          </a>
        </nav>
      </article>

      <aside v-if="headings.length > 0" class="doc-outline" :aria-label="text.onThisPage">
        <p class="doc-outline-label">{{ text.onThisPage }}</p>
        <PageOutline :headings="headings" :active-id="activeHeadingId" variant="aside" />
      </aside>
    </div>
  </div>
</template>

<style scoped>
.doc-columns {
  display: grid;
  grid-template-columns: minmax(0, var(--content-width)) var(--outline-width);
  gap: 72px;
  padding: 48px 0 96px;
}

.doc-article {
  min-width: 0;
}

.doc-band {
  margin-bottom: 36px;
}

.doc-content {
  animation: content-rise 260ms cubic-bezier(0.2, 0.8, 0.2, 1);
}

@keyframes content-rise {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
  to {
    opacity: 1;
    transform: none;
  }
}

.doc-outline {
  position: sticky;
  top: calc(var(--top-bar-height) + 48px);
  align-self: start;
  max-height: calc(100dvh - var(--top-bar-height) - 96px);
  overflow-y: auto;
  scrollbar-width: none;
}

.doc-outline-label {
  margin: 0 0 12px;
  color: var(--ink);
  font-family: var(--font-display);
  font-size: 0.9375rem;
}

.doc-pager {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
  margin-top: 72px;
  padding-top: 24px;
  border-top: 1px solid var(--rule);
}

.doc-pager-link {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 14px 16px;
  border: 1px solid var(--rule);
  border-radius: 12px;
  color: var(--ink);
  text-decoration: none;
}

.doc-pager-link:hover {
  border-color: var(--rule-strong);
  background: var(--paper-raised);
}

.doc-pager-link--next {
  grid-column: 2;
  text-align: right;
}

.doc-pager-label {
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  letter-spacing: 0.09em;
  text-transform: uppercase;
}

.doc-pager-title {
  font-family: var(--font-display);
  font-size: 1.125rem;
}

.outline-bar {
  display: none;
}

@media (max-width: 1279px) {
  .doc-columns {
    grid-template-columns: minmax(0, var(--content-width));
  }

  .doc-outline {
    display: none;
  }

  .outline-bar {
    position: sticky;
    top: var(--top-bar-height);
    z-index: 20;
    display: block;
    margin: 0 calc(var(--site-gutter) * -1);
    padding: 0 var(--site-gutter);
    border-bottom: 1px solid var(--rule);
    background: color-mix(in srgb, var(--paper) 92%, transparent);
    backdrop-filter: blur(10px);
    -webkit-backdrop-filter: blur(10px);
  }

  .outline-bar-toggle {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 44px;
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--ink-soft);
    font-family: var(--font-display);
    font-size: 0.9375rem;
    cursor: pointer;
  }

  .outline-bar-toggle svg {
    width: 14px;
    height: 14px;
    fill: none;
    stroke: currentColor;
    stroke-width: 1.6;
    transition: transform 200ms ease-out;
  }

  .outline-bar-toggle[aria-expanded="true"] svg {
    transform: rotate(180deg);
  }

  .outline-bar-menu {
    position: absolute;
    top: 100%;
    left: var(--site-gutter);
    right: var(--site-gutter);
    max-width: 420px;
    max-height: 60dvh;
    overflow-y: auto;
    padding: 12px 8px;
    border: 1px solid var(--rule);
    border-radius: 12px;
    background: var(--paper-raised);
    box-shadow: 0 12px 32px -12px color-mix(in srgb, var(--ink) 22%, transparent);
  }

  .doc-columns {
    padding-top: 32px;
  }
}

@media (max-width: 719px) {
  .doc-pager {
    grid-template-columns: 1fr;
  }

  .doc-pager-link--next {
    grid-column: 1;
  }
}
</style>
