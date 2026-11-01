import js from "@eslint/js"
import globals from "globals"
import reactHooks from "eslint-plugin-react-hooks"
import reactRefresh from "eslint-plugin-react-refresh"
import tseslint from "typescript-eslint"
import { defineConfig, globalIgnores } from "eslint/config"

export default defineConfig([
  // protocol.gen.ts 由 internal/protocolgen 生成
  globalIgnores(["out", "src/lib/api/protocol.gen.ts"]),
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      globals: { ...globals.browser, ...globals.node },
    },
  },
  {
    files: ["src/components/ui/*.{ts,tsx}"],
    rules: { "react-refresh/only-export-components": "off" },
  },
  {
    // 保留 shadcn CLI 生成的 hook，升级时继续由官方维护。
    files: ["src/hooks/use-mobile.ts"],
    rules: { "react-hooks/set-state-in-effect": "off" },
  },
])
