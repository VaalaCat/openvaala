<script setup lang="ts">
import localSearchIndex from "@localSearchIndex"
import MiniSearch, { type SearchResult } from "minisearch"
import { computed, markRaw, nextTick, ref, shallowRef, watch } from "vue"
import { useData, useRouter } from "vitepress"
import { tokenizeSearchText } from "../search-tokenize"
import { useSiteText } from "../site"

const props = defineProps<{ isOpen: boolean }>()
const emit = defineEmits<{ (e: "close"): void }>()

// SearchDocument 是 VitePress 本地搜索索引里的一节：title 是节标题，titles 是它上层的标题链。
interface SearchDocument {
  title: string
  titles: string[]
  text: string
}

type SearchHit = SearchResult & SearchDocument

const { localeIndex } = useData()
const router = useRouter()
const text = useSiteText()

// 索引按语言懒加载：@localSearchIndex 导出 { root: () => import(...), zh: () => import(...) }，
// 每个模块的 default 是 MiniSearch 序列化后的 JSON 字符串。
const searchIndexLoaders = shallowRef(localSearchIndex)
const loadedSearchIndexes = new Map<string, Promise<MiniSearch<SearchDocument>>>()

if (import.meta.hot) {
  import.meta.hot.accept("/@localSearchIndex", (module) => {
    if (!module) return
    searchIndexLoaders.value = module.default
    loadedSearchIndexes.clear()
    runSearch()
  })
}

// loadSearchIndex 加载一种语言的索引；切词和存储字段必须与 config.ts 建索引时一致。
function loadSearchIndex(locale: string): Promise<MiniSearch<SearchDocument>> {
  let searchIndex = loadedSearchIndexes.get(locale)
  if (!searchIndex) {
    searchIndex = searchIndexLoaders.value[locale]().then((module: { default: string }) =>
      markRaw(
        MiniSearch.loadJSON<SearchDocument>(module.default, {
          fields: ["title", "titles", "text"],
          storeFields: ["title", "titles", "text"],
          tokenize: tokenizeSearchText,
          searchOptions: {
            tokenize: tokenizeSearchText,
            // 多个词都要命中；中文切出的单字词不会再把只含其中一个字的段落带进结果。
            combineWith: "AND",
            prefix: true,
            fuzzy: 0.2,
            boost: { title: 4, text: 2, titles: 1 },
          },
        }),
      ),
    )
    loadedSearchIndexes.set(locale, searchIndex)
  }
  return searchIndex
}

const dialogEl = ref<HTMLElement>()
const inputEl = ref<HTMLInputElement>()
const closeButtonEl = ref<HTMLButtonElement>()
const resultsEl = ref<HTMLElement>()

const query = ref("")
const hits = shallowRef<SearchHit[]>([])
const selectedIndex = ref(0)
const hasSearched = ref(false)

// 每次搜索取一个序号，异步加载索引返回时丢弃过期结果。
let searchSequence = 0

async function runSearch() {
  const sequence = ++searchSequence
  const trimmedQuery = query.value.trim()
  if (!trimmedQuery) {
    hits.value = []
    hasSearched.value = false
    return
  }
  const searchIndex = await loadSearchIndex(localeIndex.value)
  if (sequence !== searchSequence) return
  hits.value = searchIndex.search(trimmedQuery).slice(0, 12) as SearchHit[]
  selectedIndex.value = 0
  hasSearched.value = true
}

watch(query, runSearch)
watch(localeIndex, runSearch)

// ---------- 打开与关闭 ----------

let focusBeforeOpen: HTMLElement | null = null

watch(
  () => props.isOpen,
  (isOpen) => {
    document.documentElement.classList.toggle("is-search-open", isOpen)
    if (isOpen) {
      focusBeforeOpen = document.activeElement as HTMLElement | null
      loadSearchIndex(localeIndex.value)
      nextTick(() => {
        inputEl.value?.focus()
        inputEl.value?.select()
      })
      return
    }
    focusBeforeOpen?.focus()
  },
)

function openHit(hit: SearchHit | undefined) {
  if (!hit) return
  router.go(hit.id)
  emit("close")
}

// handleKeydown 处理上下键切换、回车打开、Esc 关闭，并把 Tab 焦点留在弹窗内。
function handleKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") {
    event.preventDefault()
    emit("close")
    return
  }
  if (event.key === "Tab") {
    event.preventDefault()
    const nextFocus = document.activeElement === inputEl.value ? closeButtonEl.value : inputEl.value
    nextFocus?.focus()
    return
  }
  if (hits.value.length === 0) return
  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    event.preventDefault()
    const step = event.key === "ArrowDown" ? 1 : -1
    selectedIndex.value = (selectedIndex.value + step + hits.value.length) % hits.value.length
    nextTick(() => {
      resultsEl.value?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: "nearest" })
    })
    return
  }
  if (event.key === "Enter" && !event.isComposing && event.target === inputEl.value) {
    event.preventDefault()
    openHit(hits.value[selectedIndex.value])
  }
}

// ---------- 结果展示 ----------

const resultsSummary = computed(() => {
  if (!hasSearched.value) return ""
  return hits.value.length === 0 ? `${text.value.searchNoResults} “${query.value.trim()}”` : `${hits.value.length}`
})

// 索引正文来自渲染后的 HTML，去掉了标签但保留实体（如 &#x3C;），展示前先还原成字符。
const namedEntities: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " " }

function decodeEntities(value: string): string {
  return value.replace(/&(#x[0-9a-f]+|#\d+|[a-z]+);/gi, (entity, code: string) => {
    if (code[0] !== "#") return namedEntities[code.toLowerCase()] ?? entity
    const point = code[1].toLowerCase() === "x" ? Number.parseInt(code.slice(2), 16) : Number.parseInt(code.slice(1), 10)
    return Number.isFinite(point) ? String.fromCodePoint(point) : entity
  })
}

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (char) => `&#${char.charCodeAt(0)};`)
}

function getTermPattern(terms: string[]): RegExp | undefined {
  const escapedTerms = [...new Set(terms)]
    .filter(Boolean)
    .sort((a, b) => b.length - a.length)
    .map((term) => term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
  return escapedTerms.length > 0 ? new RegExp(`(${escapedTerms.join("|")})`, "gi") : undefined
}

// highlightTerms 转义文本后把命中的词包进 <mark>。
function highlightTerms(value: string, terms: string[]): string {
  const pattern = getTermPattern(terms)
  const escaped = escapeHtml(value)
  return pattern ? escaped.replace(pattern, "<mark>$1</mark>") : escaped
}

// getSnippet 从正文中截取第一个命中词附近的一段。
function getSnippet(hit: SearchHit): string {
  const body = decodeEntities(hit.text ?? "").replace(/\s+/g, " ").trim()
  if (!body) return ""
  const lowerBody = body.toLowerCase()
  const firstMatch = Math.min(
    ...hit.terms.map((term) => lowerBody.indexOf(term.toLowerCase())).filter((position) => position >= 0),
  )
  const anchor = Number.isFinite(firstMatch) ? firstMatch : 0
  const start = Math.max(0, anchor - 48)
  const end = Math.min(body.length, start + 168)
  const excerpt = `${start > 0 ? "…" : ""}${body.slice(start, end)}${end < body.length ? "…" : ""}`
  return highlightTerms(excerpt, hit.terms)
}

function getBreadcrumb(hit: SearchHit): string {
  return hit.titles.join(" / ")
}
</script>

<template>
  <Teleport to="body">
    <Transition name="search" :duration="{ enter: 220, leave: 140 }">
      <div v-if="isOpen" class="search-layer" @keydown="handleKeydown">
        <div class="search-backdrop" aria-hidden="true" @click="emit('close')" />
        <div ref="dialogEl" class="search-dialog" role="dialog" aria-modal="true" :aria-label="text.searchLabel">
          <div class="search-field">
            <svg class="search-field-icon" viewBox="0 0 20 20" aria-hidden="true">
              <circle cx="8.5" cy="8.5" r="5.25" />
              <path d="m12.5 12.5 4 4" />
            </svg>
            <input
              ref="inputEl"
              v-model="query"
              class="search-input"
              type="search"
              role="combobox"
              autocomplete="off"
              spellcheck="false"
              aria-autocomplete="list"
              aria-controls="search-results"
              :aria-expanded="hits.length > 0"
              :aria-activedescendant="hits.length > 0 ? `search-result-${selectedIndex}` : undefined"
              :aria-label="text.searchLabel"
              :placeholder="text.searchPlaceholder"
            />
            <button ref="closeButtonEl" class="search-close" type="button" :aria-label="text.closeSearch" @click="emit('close')">
              <span class="search-close-key">esc</span>
              <span class="search-close-label">{{ text.searchClose }}</span>
            </button>
          </div>

          <div class="search-body">
            <p v-if="!hasSearched" class="search-message">{{ text.searchEmptyQuery }}</p>
            <p v-else-if="hits.length === 0" class="search-message">{{ resultsSummary }}</p>
            <ul v-else id="search-results" ref="resultsEl" class="search-results" role="listbox" :aria-label="text.searchLabel">
              <li v-for="(hit, index) in hits" :key="hit.id" role="presentation">
                <a
                  :id="`search-result-${index}`"
                  class="search-result"
                  role="option"
                  tabindex="-1"
                  :href="hit.id"
                  :aria-selected="index === selectedIndex"
                  @mousemove="selectedIndex = index"
                  @click="emit('close')"
                >
                  <span v-if="hit.titles.length > 0" class="search-result-path" v-html="highlightTerms(getBreadcrumb(hit), hit.terms)" />
                  <span class="search-result-title" v-html="highlightTerms(hit.title, hit.terms)" />
                  <span v-if="hit.text" class="search-result-snippet" v-html="getSnippet(hit)" />
                </a>
              </li>
            </ul>
            <p class="visually-hidden" aria-live="polite">{{ resultsSummary }}</p>
          </div>

          <footer class="search-footer" aria-hidden="true">
            <span><kbd>↑</kbd><kbd>↓</kbd>{{ text.searchNavigate }}</span>
            <span><kbd>↵</kbd>{{ text.searchOpen }}</span>
            <span><kbd>esc</kbd>{{ text.searchClose }}</span>
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.search-layer {
  position: fixed;
  inset: 0;
  z-index: 100;
  display: flex;
  justify-content: center;
  align-items: flex-start;
  padding: min(14vh, 120px) 16px 16px;
}

.search-backdrop {
  position: absolute;
  inset: 0;
  background: color-mix(in srgb, var(--paper) 55%, transparent);
  backdrop-filter: blur(6px);
  -webkit-backdrop-filter: blur(6px);
}

.search-dialog {
  position: relative;
  display: flex;
  flex-direction: column;
  width: min(640px, 100%);
  max-height: min(560px, calc(100dvh - min(14vh, 120px) - 16px));
  border: 1px solid var(--rule-strong);
  border-radius: 16px;
  background: var(--paper-raised);
  box-shadow:
    0 1px 2px color-mix(in srgb, var(--ink) 6%, transparent),
    0 24px 64px -16px color-mix(in srgb, var(--ink) 28%, transparent);
  overflow: hidden;
}

.search-field {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 0 12px 0 18px;
  border-bottom: 1px solid var(--rule);
}

.search-field-icon {
  flex: none;
  width: 18px;
  height: 18px;
  fill: none;
  stroke: var(--ink-muted);
  stroke-width: 1.6;
  stroke-linecap: round;
}

.search-input {
  flex: 1;
  min-width: 0;
  height: 56px;
  border: 0;
  outline: none;
  background: transparent;
  color: var(--ink);
  font: inherit;
  font-size: 1.0625rem;
}

.search-input::placeholder {
  color: var(--ink-muted);
}

.search-input::-webkit-search-cancel-button {
  display: none;
}

.search-close {
  padding: 3px 7px;
  border: 1px solid var(--rule);
  border-radius: 6px;
  background: var(--paper);
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  letter-spacing: 0.06em;
  cursor: pointer;
}

.search-close:hover {
  color: var(--ink);
  border-color: var(--rule-strong);
}

.search-close-label {
  display: none;
}

.search-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 8px;
}

.search-message {
  margin: 0;
  padding: 28px 12px;
  color: var(--ink-muted);
  font-size: 0.9375rem;
  text-align: center;
}

.search-results {
  margin: 0;
  padding: 0;
  list-style: none;
}

.search-result {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 10px 14px 10px 22px;
  border-radius: 10px;
  color: var(--ink-soft);
  text-decoration: none;
}

.search-result[aria-selected="true"] {
  background: var(--paper-sunken);
}

/* 选中项用与侧栏当前页相同的 ▍ 光标条标出。 */
.search-result[aria-selected="true"]::before {
  content: "";
  position: absolute;
  top: 12px;
  bottom: 12px;
  left: 8px;
  width: 3px;
  background: var(--signal);
}

.search-result-path {
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  letter-spacing: 0.04em;
}

.search-result-title {
  color: var(--ink);
  font-size: 0.9375rem;
  font-weight: 500;
}

.search-result-snippet {
  display: -webkit-box;
  overflow: hidden;
  color: var(--ink-muted);
  font-size: 0.8125rem;
  line-height: 1.55;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}

.search-result :deep(mark) {
  background: transparent;
  color: var(--ink);
  font-weight: 600;
  text-decoration: underline;
  text-decoration-color: var(--signal);
  text-decoration-thickness: 1.5px;
  text-underline-offset: 3px;
}

.search-footer {
  display: flex;
  gap: 18px;
  padding: 10px 18px;
  border-top: 1px solid var(--rule);
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  letter-spacing: 0.04em;
}

.search-footer span {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}

.search-footer kbd {
  min-width: 18px;
  padding: 1px 5px;
  border: 1px solid var(--rule);
  border-radius: 5px;
  background: var(--paper);
  font-family: inherit;
  text-align: center;
}

.search-footer kbd + kbd {
  margin-left: -1px;
}

/* 打开：背景模糊层淡入，面板从 97% 缩放淡入；关闭时反向，时长更短。 */
.search-enter-active .search-backdrop,
.search-enter-active .search-dialog {
  transition: opacity 200ms ease-out, transform 220ms cubic-bezier(0.2, 0.8, 0.2, 1);
}

.search-leave-active .search-backdrop,
.search-leave-active .search-dialog {
  transition: opacity 140ms ease-in, transform 140ms ease-in;
}

.search-enter-from .search-backdrop,
.search-leave-to .search-backdrop {
  opacity: 0;
}

.search-enter-from .search-dialog,
.search-leave-to .search-dialog {
  opacity: 0;
  transform: translateY(-4px) scale(0.97);
}

@media (max-width: 719px) {
  .search-layer {
    padding: 0;
  }

  .search-dialog {
    width: 100%;
    height: 100dvh;
    max-height: 100dvh;
    border: 0;
    border-radius: 0;
  }

  .search-field {
    padding-top: env(safe-area-inset-top);
  }

  .search-body {
    padding-bottom: max(8px, env(safe-area-inset-bottom));
  }

  .search-close {
    min-width: 44px;
    min-height: 44px;
    border: 0;
    background: transparent;
    font-family: var(--font-text);
    font-size: 0.8125rem;
  }

  .search-close-key {
    display: none;
  }

  .search-close-label {
    display: inline;
  }

  .search-footer {
    display: none;
  }
}
</style>
