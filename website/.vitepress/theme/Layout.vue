<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue"
import { useData, useRoute, withBase } from "vitepress"
import DocPage from "./components/DocPage.vue"
import HomePage from "./components/HomePage.vue"
import Search from "./components/Search.vue"
import SiteSidebar from "./components/SiteSidebar.vue"
import TopBar from "./components/TopBar.vue"
import { useSiteText } from "./site"

const { frontmatter, page, localeIndex } = useData()
const route = useRoute()
const text = useSiteText()

const isHome = computed(() => frontmatter.value.layout === "home" && !page.value.isNotFound)
const localeHomeLink = computed(() => withBase(localeIndex.value === "zh" ? "/zh/" : "/"))

// 搜索弹窗和移动端侧栏抽屉的开关由 Layout 持有，顶栏只发出打开请求。
const isSearchOpen = ref(false)
const isSidebarOpen = ref(false)

watch(
  () => route.path,
  () => {
    isSidebarOpen.value = false
  },
)

// handleGlobalKeydown 处理 Ctrl/Cmd K 打开或关闭搜索。
function handleGlobalKeydown(event: KeyboardEvent) {
  if (event.key.toLowerCase() !== "k" || !(event.metaKey || event.ctrlKey) || event.altKey) return
  event.preventDefault()
  isSearchOpen.value = !isSearchOpen.value
}

onMounted(() => window.addEventListener("keydown", handleGlobalKeydown))
onUnmounted(() => window.removeEventListener("keydown", handleGlobalKeydown))
</script>

<template>
  <div class="site-shell" :class="{ 'is-home': isHome }">
    <a class="skip-link" href="#main">{{ text.skipToContent }}</a>

    <TopBar
      :is-sidebar-open="isSidebarOpen"
      @open-search="isSearchOpen = true"
      @toggle-sidebar="isSidebarOpen = !isSidebarOpen"
    />

    <div class="site-body" :class="isHome ? 'site-body--home' : 'site-body--doc'">
      <SiteSidebar :is-open="isSidebarOpen" :is-drawer-only="isHome" @close="isSidebarOpen = false" />

      <main id="main" class="site-main" tabindex="-1">
        <HomePage v-if="isHome" />
        <article v-else-if="page.isNotFound" class="not-found">
          <p class="eyebrow">404</p>
          <h1>{{ text.notFoundTitle }}</h1>
          <p>{{ text.notFoundBody }}</p>
          <p><a class="text-link" :href="localeHomeLink">{{ text.notFoundAction }}</a></p>
        </article>
        <DocPage v-else />
      </main>
    </div>

    <footer class="site-footer">
      <p>{{ text.footer }}</p>
    </footer>

    <Search :is-open="isSearchOpen" @close="isSearchOpen = false" />
  </div>
</template>
