import { createInstance } from "i18next"
import { initReactI18next } from "react-i18next"
import zhCN from "./locales/zh-CN.json"
import en from "./locales/en.json"

export type Locale = "zh-CN" | "en"
export const supportedLanguages: readonly { value: Locale; label: string }[] = [
  { value: "zh-CN", label: "简体中文" },
  { value: "en", label: "English" },
]
export const i18n = createInstance().use(initReactI18next)
const preferenceKey = "openvaala.language"

// 先用保存的语言，再按浏览器语言的顺序找第一个支持的语言；中文一律用简体，都不支持时用简体中文
export function resolveLanguage(
  saved: string | null,
  browserLanguages: readonly string[]
): Locale {
  if (supportedLanguages.some(({ value }) => value === saved))
    return saved as Locale
  for (const language of browserLanguages) {
    const primary = language.toLowerCase().split("-")[0]
    if (primary === "zh") return "zh-CN"
    if (primary === "en") return "en"
  }
  return "zh-CN"
}

i18n.on("languageChanged", (language) => {
  if (typeof document !== "undefined") document.documentElement.lang = language
  try {
    window.localStorage.setItem(preferenceKey, language)
  } catch {
    // 浏览器禁用存储时，语言切换仍在本次会话生效。
  }
})

// 读取保存的语言并初始化翻译资源
export async function initializeI18n(): Promise<void> {
  let saved: string | null = null
  try {
    saved = window.localStorage.getItem(preferenceKey)
  } catch {
    // 无法读取偏好时，继续使用浏览器语言。
  }
  await i18n.init({
    resources: {
      "zh-CN": { translation: zhCN },
      en: { translation: en },
    },
    lng: resolveLanguage(
      saved,
      typeof navigator === "undefined" ? [] : navigator.languages
    ),
    supportedLngs: supportedLanguages.map(({ value }) => value),
    fallbackLng: "zh-CN",
    returnNull: false,
    interpolation: { escapeValue: false },
  })
}
