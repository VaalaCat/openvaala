import "i18next"
import type translation from "./locales/zh-CN.json"

declare module "i18next" {
  interface CustomTypeOptions {
    defaultNS: "translation"
    returnNull: false
    resources: { translation: typeof translation }
  }
}
