import { computed, onMounted, onUnmounted, ref } from "vue"
import { useData } from "vitepress"
import type { SidebarGroup, SidebarItem, ThemeConfig } from "./types"

// ---------- 界面文案 ----------

const siteTextByLocale = {
  root: {
    skipToContent: "Skip to content",
    search: "Search",
    searchLabel: "Search the docs",
    searchPlaceholder: "Search pages and sections",
    searchEmptyQuery: "Type to search titles, sections and text.",
    searchNoResults: "No results for",
    searchNavigate: "navigate",
    searchOpen: "open",
    searchClose: "close",
    closeSearch: "Close search",
    menu: "Menu",
    closeMenu: "Close menu",
    docsNavigation: "Documentation",
    onThisPage: "On this page",
    previous: "Previous",
    next: "Next",
    switchLanguage: "中文",
    themeLabel: { light: "Light theme", dark: "Dark theme", auto: "System theme" },
    themeNext: "Switch theme",
    github: "OpenVaala on GitHub",
    copy: "Copy",
    copied: "Copied",
    copyInstall: "Copy install command",
    heroImagePending: "Product screenshot",
    notFoundTitle: "This page does not exist",
    notFoundBody: "The link may be outdated, or the page moved during a rename.",
    notFoundAction: "Back to the docs home",
    footer: "MIT License · © 2026 VaalaCat",
  },
  zh: {
    skipToContent: "跳到正文",
    search: "搜索",
    searchLabel: "搜索文档",
    searchPlaceholder: "搜索页面和章节",
    searchEmptyQuery: "输入关键词，搜索标题、章节和正文。",
    searchNoResults: "没有找到",
    searchNavigate: "切换",
    searchOpen: "打开",
    searchClose: "关闭",
    closeSearch: "关闭搜索",
    menu: "目录",
    closeMenu: "关闭目录",
    docsNavigation: "文档",
    onThisPage: "本页内容",
    previous: "上一页",
    next: "下一页",
    switchLanguage: "English",
    themeLabel: { light: "浅色主题", dark: "深色主题", auto: "跟随系统" },
    themeNext: "切换主题",
    github: "OpenVaala 的 GitHub 仓库",
    copy: "复制",
    copied: "已复制",
    copyInstall: "复制安装命令",
    heroImagePending: "产品截图",
    notFoundTitle: "这个页面不存在",
    notFoundBody: "链接可能已经过期，或者页面在改名时移动了位置。",
    notFoundAction: "回到文档首页",
    footer: "MIT License · © 2026 VaalaCat",
  },
}

export type SiteText = (typeof siteTextByLocale)["root"]

// useSiteText 返回当前语言的界面文案。
export function useSiteText() {
  const { localeIndex } = useData()
  return computed<SiteText>(() => (localeIndex.value === "zh" ? siteTextByLocale.zh : siteTextByLocale.root))
}

// ---------- 页面位置 ----------

// getPageLink 把页面源文件路径换成不带 base 的站内链接，例如 zh/guides/web.md -> /zh/guides/web。
export function getPageLink(relativePath: string): string {
  return "/" + relativePath.replace(/(^|\/)index\.md$/, "$1").replace(/\.md$/, "")
}

// usePagePosition 返回当前页面在侧栏中的分组、前一页和后一页。
export function usePagePosition() {
  const { page, theme } = useData<ThemeConfig>()
  const currentLink = computed(() => getPageLink(page.value.relativePath))

  return computed(() => {
    const groups: SidebarGroup[] = theme.value.sidebar ?? []
    const items: { group: SidebarGroup; item: SidebarItem }[] = groups.flatMap((group) =>
      group.items.map((item) => ({ group, item })),
    )
    const index = items.findIndex(({ item }) => item.link === currentLink.value)
    return {
      currentLink: currentLink.value,
      group: index >= 0 ? items[index].group : undefined,
      previous: index > 0 ? items[index - 1].item : undefined,
      next: index >= 0 && index < items.length - 1 ? items[index + 1].item : undefined,
    }
  })
}

// useAlternateLocaleLink 返回当前页面在另一种语言下的链接；docs/zh 与英文页面路径一一对应。
export function useAlternateLocaleLink() {
  const { page, localeIndex } = useData()
  return computed(() => {
    if (page.value.isNotFound) return localeIndex.value === "zh" ? "/" : "/zh/"
    const relativePath = page.value.relativePath
    const target = localeIndex.value === "zh" ? relativePath.replace(/^zh\//, "") : `zh/${relativePath}`
    return getPageLink(target)
  })
}

// ---------- 外观 ----------

export type AppearanceMode = "light" | "dark" | "auto"

// VitePress 首屏脚本和内置 useDark 共用这个存储键，三者保持同一份外观设置。
const appearanceStorageKey = "vitepress-theme-appearance"

// useAppearanceMode 读写浅色、深色、跟随系统三态外观设置。
// 写入后派发 storage 事件，由 VitePress 内置的 useDark 同步 html.dark 类。
export function useAppearanceMode() {
  const mode = ref<AppearanceMode>("auto")

  function readStoredMode(): AppearanceMode {
    const stored = localStorage.getItem(appearanceStorageKey)
    return stored === "light" || stored === "dark" ? stored : "auto"
  }

  function setMode(next: AppearanceMode) {
    const oldValue = localStorage.getItem(appearanceStorageKey)
    localStorage.setItem(appearanceStorageKey, next)
    window.dispatchEvent(
      new StorageEvent("storage", { key: appearanceStorageKey, oldValue, newValue: next, storageArea: localStorage }),
    )
    mode.value = next
  }

  function handleStorage(event: StorageEvent) {
    if (event.key === appearanceStorageKey) mode.value = readStoredMode()
  }

  onMounted(() => {
    mode.value = readStoredMode()
    window.addEventListener("storage", handleStorage)
  })
  onUnmounted(() => window.removeEventListener("storage", handleStorage))

  return { mode, setMode }
}
