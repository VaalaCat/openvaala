import type { CSSProperties } from "react"
import type { HighlighterCore, LanguageRegistration } from "shiki/core"
import { shouldHighlight, type HighlightedLine } from "./highlighter-types"

const themes = { light: "github-light", dark: "github-dark" }

type LanguageModule = { default: LanguageRegistration[] }

// 支持着色的语言，键是 shiki 的语言名；用到时才加载
const languageLoaders: Record<string, () => Promise<LanguageModule>> = {
  c: () => import("shiki/langs/c.mjs"),
  cpp: () => import("shiki/langs/cpp.mjs"),
  csharp: () => import("shiki/langs/csharp.mjs"),
  css: () => import("shiki/langs/css.mjs"),
  diff: () => import("shiki/langs/diff.mjs"),
  docker: () => import("shiki/langs/docker.mjs"),
  go: () => import("shiki/langs/go.mjs"),
  html: () => import("shiki/langs/html.mjs"),
  ini: () => import("shiki/langs/ini.mjs"),
  java: () => import("shiki/langs/java.mjs"),
  javascript: () => import("shiki/langs/javascript.mjs"),
  json: () => import("shiki/langs/json.mjs"),
  jsonc: () => import("shiki/langs/jsonc.mjs"),
  jsx: () => import("shiki/langs/jsx.mjs"),
  kotlin: () => import("shiki/langs/kotlin.mjs"),
  lua: () => import("shiki/langs/lua.mjs"),
  make: () => import("shiki/langs/make.mjs"),
  markdown: () => import("shiki/langs/markdown.mjs"),
  php: () => import("shiki/langs/php.mjs"),
  powershell: () => import("shiki/langs/powershell.mjs"),
  proto: () => import("shiki/langs/proto.mjs"),
  python: () => import("shiki/langs/python.mjs"),
  ruby: () => import("shiki/langs/ruby.mjs"),
  rust: () => import("shiki/langs/rust.mjs"),
  scss: () => import("shiki/langs/scss.mjs"),
  shellscript: () => import("shiki/langs/shellscript.mjs"),
  sql: () => import("shiki/langs/sql.mjs"),
  swift: () => import("shiki/langs/swift.mjs"),
  toml: () => import("shiki/langs/toml.mjs"),
  tsx: () => import("shiki/langs/tsx.mjs"),
  typescript: () => import("shiki/langs/typescript.mjs"),
  xml: () => import("shiki/langs/xml.mjs"),
  yaml: () => import("shiki/langs/yaml.mjs"),
}

// 常见别名和文件扩展名对应的语言名，调用方可以直接传扩展名
const languageAliases: Record<string, string> = {
  bash: "shellscript",
  cc: "cpp",
  cjs: "javascript",
  cs: "csharp",
  cts: "typescript",
  cxx: "cpp",
  dockerfile: "docker",
  golang: "go",
  h: "c",
  hh: "cpp",
  hpp: "cpp",
  htm: "html",
  js: "javascript",
  kt: "kotlin",
  kts: "kotlin",
  makefile: "make",
  md: "markdown",
  mjs: "javascript",
  mts: "typescript",
  patch: "diff",
  properties: "ini",
  ps1: "powershell",
  py: "python",
  rb: "ruby",
  rs: "rust",
  sh: "shellscript",
  shell: "shellscript",
  svg: "xml",
  ts: "typescript",
  yml: "yaml",
  zsh: "shellscript",
}

// 全站共用的高亮器，第一次着色时创建
let highlighterPromise: Promise<HighlighterCore> | undefined

// 创建高亮器：shiki 本身、正则引擎和两套主题都按需加载，不进页面的初始代码
async function createHighlighter(): Promise<HighlighterCore> {
  const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] =
    await Promise.all([import("shiki/core"), import("shiki/engine/javascript")])
  return createHighlighterCore({
    themes: [
      import("shiki/themes/github-light.mjs"),
      import("shiki/themes/github-dark.mjs"),
    ],
    engine: createJavaScriptRegexEngine({ forgiving: true }),
  })
}

// 按语言给文本着色并按行返回；文本太大、语言不支持或加载失败时返回 undefined，调用方按纯文本显示
export async function highlightLines(
  text: string,
  lang: string
): Promise<HighlightedLine[] | undefined> {
  const language = languageAliases[lang.toLowerCase()] ?? lang.toLowerCase()
  const loadLanguage = languageLoaders[language]
  if (!loadLanguage || !shouldHighlight(text)) return undefined
  try {
    // 1. 第一次用时创建高亮器，创建失败时下次再试；第一次用到某种语言时加载它
    highlighterPromise ??= createHighlighter().catch((error: unknown) => {
      highlighterPromise = undefined
      throw error
    })
    const highlighter = await highlighterPromise
    if (!highlighter.getLoadedLanguages().includes(language))
      await highlighter.loadLanguage(loadLanguage())
    // 2. 两套主题一起着色，颜色只放进 CSS 变量，切换主题不需要重新着色
    const { tokens } = highlighter.codeToTokens(text, {
      lang: language,
      themes,
      defaultColor: false,
    })
    return tokens.map((line) =>
      line.map((token) => ({
        content: token.content,
        style: token.htmlStyle as CSSProperties | undefined,
      }))
    )
  } catch {
    return undefined
  }
}
