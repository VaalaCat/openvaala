import fs from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { defineConfigWithTheme } from "vitepress"
import { codeThemeDark, codeThemeLight } from "./code-themes"
import { isSceneName } from "./theme/scenes/scene-names"
import { tokenizeSearchText } from "./theme/search-tokenize"
import type { SidebarGroup, ThemeConfig } from "./theme/types"

const websiteDir = path.dirname(fileURLToPath(import.meta.url))
const docsDir = path.resolve(websiteDir, "../../docs")
const base = process.env.DOCS_BASE ?? "/"

// 侧栏四组页面的顺序；页面标题在构建时从各页 H1 读取，改名不会和侧栏脱节。
const sidebarSections = [
  {
    label: { en: "Getting started", zh: "入门" },
    pages: ["getting-started/quickstart", "getting-started/install"],
  },
  {
    label: { en: "Guides", zh: "使用" },
    pages: ["guides/terminal", "guides/web", "guides/desktop", "guides/editors", "guides/codex", "guides/conversations"],
  },
  {
    label: { en: "Configuration", zh: "配置" },
    pages: [
      "configuration/overview",
      "configuration/models",
      "configuration/agents",
      "configuration/executors",
      "configuration/tools",
      "configuration/extensions",
      "configuration/telemetry",
    ],
  },
  {
    label: { en: "Reference", zh: "参考" },
    pages: ["reference/cli", "reference/architecture"],
  },
]

// GitHub 提示块在中文页面使用的默认标题；作者自己写了标题的提示块保持原样。
const zhAlertTitles: Record<string, string> = {
  NOTE: "说明",
  TIP: "提示",
  IMPORTANT: "重要",
  WARNING: "警告",
  CAUTION: "注意",
  INFO: "信息",
  DANGER: "危险",
}

// 正文场景标记：<!-- scene: name | 图注 --> 紧跟一个 ```text 代码块（ASCII 示意图）。
// GitHub 和内置 skill 里注释不可见、显示 ASCII 图；站点上换成动画 <Scene>，ASCII 留作读屏描述。
const sceneMarkerPattern = /^<!--\s*scene:\s*([\w-]+)\s*(?:\|\s*([\s\S]*?))?\s*-->\s*$/
const warnedSceneNames = new Set<string>()

function escapeAttribute(value: string): string {
  return value.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
}

// getPageTitle 读取页面第一个 H1 作为侧栏标题；页面缺失或没有 H1 时直接让构建失败。
function getPageTitle(file: string): string {
  const source = fs.readFileSync(file, "utf8").replace(/^---\n[\s\S]*?\n---\n/, "")
  const heading = /^#\s+(.+?)\s*(?:\{#[^}]*\})?\s*$/m.exec(source)?.[1]
  if (!heading) {
    throw new Error(`docs page ${path.relative(docsDir, file)} has no H1 title`)
  }
  return heading.replace(/`/g, "").replace(/\*\*?([^*]+)\*\*?/g, "$1")
}

// getSidebarGroups 生成一种语言的侧栏；localeDir 为 "" 表示英文根目录，"zh" 表示中文目录。
function getSidebarGroups(localeDir: "" | "zh"): SidebarGroup[] {
  const linkPrefix = localeDir ? `/${localeDir}/` : "/"
  return sidebarSections.map((section) => ({
    text: localeDir === "zh" ? section.label.zh : section.label.en,
    items: section.pages.map((page) => ({
      text: getPageTitle(path.join(docsDir, localeDir, `${page}.md`)),
      link: `${linkPrefix}${page}`,
    })),
  }))
}

export default defineConfigWithTheme<ThemeConfig>({
  title: "OpenVaala",
  // GitHub Pages 发布在 /openvaala/ 下，由 DOCS_BASE 指定；本地开发用根路径。
  base,
  srcDir: docsDir,
  srcExclude: ["SKILL.md", "superpowers/**"],
  cleanUrls: true,
  lastUpdated: false,
  // 本地服务地址会出现在使用说明里，不当作站内死链。
  ignoreDeadLinks: "localhostLinks",
  // 锚点跳转时为顶栏及窄屏页内目录留出空间。
  scrollOffset: { selector: ".top-bar", padding: 56 },
  head: [
    ["link", { rel: "icon", type: "image/svg+xml", href: `${base}favicon.svg` }],
    ["meta", { name: "theme-color", content: "#faf9f6", media: "(prefers-color-scheme: light)" }],
    ["meta", { name: "theme-color", content: "#1c1917", media: "(prefers-color-scheme: dark)" }],
  ],
  markdown: {
    theme: { light: codeThemeLight, dark: codeThemeDark },
    config(md) {
      // 首页正文前半段是给 GitHub 和内置 skill 看的概览（与首页卖点重复），站点首页只渲染
      // <!-- site-home-body --> 之后的内容；没有这行标记时渲染全部正文。
      md.core.ruler.push("openvaala-site-home-body", (state) => {
        if (state.env?.frontmatter?.layout !== "home") return
        const markerIndex = state.tokens.findIndex(
          (token) => token.type === "html_block" && token.content.trim() === "<!-- site-home-body -->",
        )
        if (markerIndex >= 0) state.tokens.splice(0, markerIndex + 1)
      })

      // 把场景标记和紧随的 ASCII 代码块换成 <Scene>；场景名未登记时保留代码块并在构建时提示。
      md.core.ruler.push("openvaala-scenes", (state) => {
        const tokens = state.tokens
        for (let index = 0; index < tokens.length - 1; index++) {
          const match = tokens[index].type === "html_block" ? sceneMarkerPattern.exec(tokens[index].content) : null
          const fence = tokens[index + 1]
          if (!match || fence.type !== "fence") continue
          const [, name, caption] = match
          if (!isSceneName(name)) {
            if (!warnedSceneNames.has(name)) console.warn(`[openvaala-docs] unknown scene "${name}" in ${state.env?.relativePath}`)
            warnedSceneNames.add(name)
            continue
          }
          const captionAttribute = caption ? ` caption="${escapeAttribute(caption)}"` : ""
          tokens[index].content =
            `<Scene name="${name}"${captionAttribute} description="${escapeAttribute(fence.content.trimEnd())}" />\n`
          tokens.splice(index + 1, 1)
        }
      })

      // 中文页面里没有自定义标题的 GitHub 提示块换成中文标题。
      md.core.ruler.after("github-alerts", "openvaala-zh-alert-titles", (state) => {
        if (!String(state.env?.relativePath ?? "").startsWith("zh/")) return
        for (const token of state.tokens) {
          if (token.type !== "github_alert_open") continue
          const title = zhAlertTitles[token.meta?.title]
          if (title) token.meta.title = title
        }
      })
    },
  },
  vite: {
    // 静态资源（favicon）放在 website/public，正文目录 docs/ 只放 Markdown 和图片。
    publicDir: path.resolve(websiteDir, "../public"),
    // 正文在 website 之外的 docs/，页面编译出的 vue 导入统一从 website/node_modules 解析。
    resolve: {
      alias: [{ find: /^vue($|\/)/, replacement: `${path.resolve(websiteDir, "../node_modules/vue")}$1` }],
    },
    server: {
      // docs/superpowers 是本地软链接，开发服务器不跟随监听。
      watch: { ignored: ["**/superpowers/**"] },
    },
  },
  themeConfig: {
    repository: "https://github.com/VaalaCat/openvaala",
    sidebar: [],
    search: {
      provider: "local",
      options: {
        miniSearch: {
          // 中文没有空格分词，建索引和查询都用 Intl.Segmenter 切词；正文存入索引用于结果摘要。
          options: { tokenize: tokenizeSearchText, storeFields: ["title", "titles", "text"] },
        },
      },
    },
  },
  locales: {
    root: {
      label: "English",
      lang: "en-US",
      description: "OpenVaala is a small, understandable agent framework written in Go, with a terminal app, web UI, desktop app and editor integrations.",
      themeConfig: { sidebar: getSidebarGroups("") },
    },
    zh: {
      label: "简体中文",
      lang: "zh-CN",
      link: "/zh/",
      description: "OpenVaala 是用 Go 构建的小型、可理解的 Agent Framework，提供终端、Web、桌面端和编辑器接入。",
      themeConfig: { sidebar: getSidebarGroups("zh") },
    },
  },
})
