import path from "node:path"
import { fileURLToPath } from "node:url"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig, loadEnv } from "vite"

const projectDirectory = path.dirname(fileURLToPath(import.meta.url))

export default defineConfig(({ mode }) => ({
  base: "/web/v1/",
  plugins: [react(), tailwindcss()],
  worker: { format: "es" },
  resolve: {
    dedupe: ["react", "react-dom"],
    alias: [
      { find: "@", replacement: path.resolve(projectDirectory, "./src") },
      // shadcn 生成的组件 import { cn } from "cn"，精确别名指向项目的 cn 配置
      {
        find: /^cn$/,
        replacement: path.resolve(projectDirectory, "./src/lib/utils.ts"),
      },
    ],
  },
  build: {
    outDir: "out/web",
    // 超过 500KB 的只有 shiki 的 C++ 语法（约 800KB），它是按需加载的 chunk；入口和页面 chunk 都在 500KB 以下
    chunkSizeWarningLimit: 800,
  },
  server: {
    proxy: {
      "/api": {
        target:
          loadEnv(mode, process.cwd(), "").VITE_SERVE_URL ||
          "http://127.0.0.1:8790",
        ws: true,
      },
    },
  },
}))
