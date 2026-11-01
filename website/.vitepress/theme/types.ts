import type { DefaultTheme } from "vitepress"

// ---------- 站点配置 ----------

// ThemeConfig 是 config.ts 的 themeConfig，按语言在 locales 中覆盖 sidebar。
export interface ThemeConfig {
  repository: string
  sidebar: SidebarGroup[]
  search: { provider: "local"; options: DefaultTheme.LocalSearchOptions }
}

// SidebarGroup 是侧栏的一组页面，text 是组名，同时作为页面 H1 上方的分组标签。
export interface SidebarGroup {
  text: string
  items: SidebarItem[]
}

// SidebarItem 的 link 不带 base，例如 /zh/guides/web。
export interface SidebarItem {
  text: string
  link: string
}

// ---------- 页面目录 ----------

// OutlineHeading 是正文里的一个 H2 或 H3，用于本页目录。
export interface OutlineHeading {
  id: string
  text: string
  level: 2 | 3
}

// ---------- 首页 frontmatter ----------

// HomeFrontmatter 是 docs/index.md 使用 layout: home 时的 frontmatter。
export interface HomeFrontmatter {
  layout: "home"
  hero?: {
    title?: string
    tagline?: string
    actions?: HomeAction[]
  }
  features?: HomeFeature[]
}

export interface HomeAction {
  text: string
  link: string
}

export interface HomeFeature {
  title: string
  details: string
  link?: string
}
