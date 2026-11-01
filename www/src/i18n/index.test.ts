import assert from "node:assert/strict"
import { test } from "node:test"
import {
  i18n,
  initializeI18n,
  resolveLanguage,
  supportedLanguages,
} from "./index"

test("resolveLanguage prefers the saved language, then browser languages, then zh-CN", () => {
  const cases: {
    name: string
    saved: string | null
    browserLanguages: string[]
    want: string
  }[] = [
    {
      name: "saved_preference_wins",
      saved: "en",
      browserLanguages: ["zh-CN"],
      want: "en",
    },
    {
      name: "chinese_region_falls_back_to_simplified",
      saved: null,
      browserLanguages: ["zh-TW"],
      want: "zh-CN",
    },
    {
      name: "english_region_uses_english",
      saved: null,
      browserLanguages: ["en-US"],
      want: "en",
    },
    {
      name: "unsupported_browser_language",
      saved: null,
      browserLanguages: ["fr"],
      want: "zh-CN",
    },
    {
      name: "unsupported_saved_language",
      saved: "fr",
      browserLanguages: [],
      want: "zh-CN",
    },
  ]
  for (const { name, saved, browserLanguages, want } of cases)
    assert.equal(resolveLanguage(saved, browserLanguages), want, name)
})

test("language changes translate text, set the document language and persist even without storage", async () => {
  let saved: string | null = "en"
  const root = { lang: "" }
  Object.assign(globalThis, {
    document: { documentElement: root },
    window: {
      localStorage: {
        getItem: () => saved,
        setItem: (_key: string, value: string) => {
          saved = value
        },
      },
    },
  })
  await initializeI18n()
  assert.equal(i18n.resolvedLanguage, "en")
  assert.equal(root.lang, "en")
  assert.equal(i18n.t("common.actions.copy"), "Copy")
  for (const { value } of supportedLanguages) {
    await i18n.changeLanguage(value)
    assert.equal(root.lang, value)
    assert.equal(saved, value)
    assert.notEqual(i18n.t("common.actions.copy"), "common.actions.copy")
  }
  window.localStorage.getItem = () => {
    throw new Error("storage denied")
  }
  window.localStorage.setItem = () => {
    throw new Error("storage denied")
  }
  await initializeI18n()
  await i18n.changeLanguage("zh-CN")
  assert.equal(root.lang, "zh-CN")
  assert.equal(i18n.t("common.actions.copy"), "复制")
})
