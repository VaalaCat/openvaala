import { persistentAtom } from "@nanostores/persistent"
import { atom, onSet } from "nanostores"

export type AppearanceMode = "light" | "dark" | "system"
export type AppearancePreferences = {
  mode: AppearanceMode
  theme: "stone" | "blue"
  bodyFont: "noto-serif" | "jetbrains-mono" | "system"
  headingFont: "noto-serif" | "jetbrains-mono" | "system"
  body: { fontSize: number; lineHeight: number; paragraphSpacing: number }
  code: {
    fontSize: number
    lineHeight: number
    paddingX: number
    paddingY: number
  }
  terminal: {
    fontSize: number
    lineHeight: number
    letterSpacing: number
    padding: number
  }
}

export const defaultAppearance: AppearancePreferences = {
  mode: "system",
  theme: "stone",
  bodyFont: "noto-serif",
  headingFont: "jetbrains-mono",
  body: { fontSize: 14, lineHeight: 24, paragraphSpacing: 12 },
  code: { fontSize: 12, lineHeight: 20, paddingX: 12, paddingY: 8 },
  terminal: { fontSize: 13, lineHeight: 1.1, letterSpacing: 0, padding: 8 },
}
// 阅读和终端数值的可选范围、步长；菜单和已保存设置使用同一份尺度。
export const appearanceNumberRanges = {
  body: {
    fontSize: { min: 10, max: 24, step: 1 },
    lineHeight: { min: 16, max: 40, step: 2 },
    paragraphSpacing: { min: 0, max: 32, step: 4 },
  },
  code: {
    fontSize: { min: 10, max: 24, step: 1 },
    lineHeight: { min: 16, max: 40, step: 2 },
    paddingX: { min: 0, max: 32, step: 4 },
    paddingY: { min: 0, max: 32, step: 4 },
  },
  terminal: {
    fontSize: { min: 10, max: 24, step: 1 },
    lineHeight: { min: 1, max: 1.6, step: 0.05 },
    letterSpacing: { min: 0, max: 4, step: 0.5 },
    padding: { min: 0, max: 32, step: 4 },
  },
} as const
const choices = {
  mode: ["light", "dark", "system"],
  theme: ["stone", "blue"],
  bodyFont: ["noto-serif", "jetbrains-mono", "system"],
  headingFont: ["noto-serif", "jetbrains-mono", "system"],
}

// 解析已保存的外观，不认识的字段值使用对应默认值。
export function parseAppearance(value: string): AppearancePreferences {
  // 1. 逐项读取合法选择。
  const appearance = {
    ...defaultAppearance,
    body: { ...defaultAppearance.body },
    code: { ...defaultAppearance.code },
    terminal: { ...defaultAppearance.terminal },
  }
  try {
    const saved = JSON.parse(value) as unknown
    if (!saved || typeof saved !== "object") return appearance
    for (const key of Object.keys(choices) as (keyof typeof choices)[]) {
      const candidate = (saved as Record<string, unknown>)[key]
      if (typeof candidate === "string" && choices[key].includes(candidate))
        Object.assign(appearance, { [key]: candidate })
    }
    // 2. 数值逐字段校验，损坏字段不影响同组合法选择。
    for (const group of ["body", "code", "terminal"] as const) {
      const values = (saved as Record<string, unknown>)[group]
      if (!values || typeof values !== "object") continue
      for (const [key, { min, max, step }] of Object.entries(
        appearanceNumberRanges[group]
      )) {
        const candidate = (values as Record<string, unknown>)[key]
        if (
          typeof candidate === "number" &&
          Number.isFinite(candidate) &&
          candidate >= min &&
          candidate <= max &&
          Math.abs(
            (candidate - min) / step - Math.round((candidate - min) / step)
          ) < 0.000001
        )
          Object.assign(appearance[group], { [key]: candidate })
      }
    }
  } catch {
    // 用默认外观
  }
  return appearance
}

// 读取本机保存的主题，存储不可用时使用系统主题。
function getSavedAppearance(): AppearancePreferences {
  // 1. 读取已保存的主题。
  try {
    const saved = localStorage.getItem("openvaala.appearance")
    if (saved) return parseAppearance(saved)
  } catch {
    // 本次使用默认外观。
  }
  return { ...defaultAppearance }
}

export const $appearance = atom<AppearancePreferences>(getSavedAppearance())

onSet($appearance, ({ newValue }) => {
  try {
    localStorage.setItem("openvaala.appearance", JSON.stringify(newValue))
  } catch {
    // 本次选择仍写入内存。
  }
})

// 在页面根节点应用明暗、配色和字体，并同步系统与其他标签页的变化。
export function initializeAppearance(): void {
  // 1. 订阅本机偏好和系统明暗变化。
  const media = window.matchMedia("(prefers-color-scheme: dark)")
  const applyAppearance = () => {
    const { mode, theme, bodyFont, headingFont, body, code, terminal } =
      $appearance.get()
    const root = document.documentElement
    root.classList.toggle(
      "dark",
      mode === "dark" || (mode === "system" && media.matches)
    )
    root.dataset.theme = theme
    root.dataset.bodyFont = bodyFont
    root.dataset.headingFont = headingFont
    root.style.setProperty("--reading-font-size", `${body.fontSize}px`)
    root.style.setProperty("--reading-line-height", `${body.lineHeight}px`)
    root.style.setProperty(
      "--reading-paragraph-spacing",
      `${body.paragraphSpacing}px`
    )
    root.style.setProperty("--code-font-size", `${code.fontSize}px`)
    root.style.setProperty("--code-line-height", `${code.lineHeight}px`)
    root.style.setProperty("--code-padding-x", `${code.paddingX}px`)
    root.style.setProperty("--code-padding-y", `${code.paddingY}px`)
    root.style.setProperty("--terminal-padding", `${terminal.padding}px`)
  }
  media.addEventListener("change", applyAppearance)
  $appearance.subscribe(applyAppearance)
  // 2. 同步其他标签页保存或清除的外观。
  window.addEventListener("storage", (event) => {
    if (event.key === "openvaala.appearance" || event.key === null)
      $appearance.set(
        event.newValue
          ? parseAppearance(event.newValue)
          : { ...defaultAppearance }
      )
  })
}

// 桌面左栏是否展开？收起时只显示图标
export const $isSidebarOpen = persistentAtom<boolean>(
  "openvaala.sidebarOpen",
  true,
  { encode: String, decode: (value) => value !== "false" }
)

// RightPanelTab 是对话页右栏的页签
export type RightPanelTab = "agents" | "context" | "turns" | "rpc" | "debug"
const rightPanelTabs: readonly RightPanelTab[] = [
  "agents",
  "context",
  "turns",
  "rpc",
  "debug",
]

// 右栏上次打开的页签；换对话、进子 Session 页时保留
export const $rightPanelTab = persistentAtom<RightPanelTab>(
  "openvaala.rightPanelTab",
  "agents",
  {
    encode: (tab) => tab,
    decode: (value) =>
      rightPanelTabs.includes(value as RightPanelTab)
        ? (value as RightPanelTab)
        : "agents",
  }
)

// 桌面对话页右栏最近一次展开时中间区和右栏的宽度比例（Panel id → 百分比）；没拖过时为空，用默认宽度
export const $conversationLayout = persistentAtom<Record<string, number>>(
  "openvaala.conversationLayout",
  {},
  { encode: JSON.stringify, decode: parseLayout }
)

// 桌面对话页的右栏是否收起？默认收起，再展开时按 $conversationLayout 的宽度恢复
export const $isRightPanelCollapsed = persistentAtom<boolean>(
  "openvaala.rightPanelCollapsed",
  true,
  { encode: String, decode: (value) => value === "true" }
)

// 存储里的布局损坏时当作没拖过
function parseLayout(value: string): Record<string, number> {
  try {
    const layout = JSON.parse(value) as unknown
    if (
      layout &&
      typeof layout === "object" &&
      Object.values(layout).every((size) => typeof size === "number")
    )
      return layout as Record<string, number>
  } catch {
    // 用默认布局
  }
  return {}
}
