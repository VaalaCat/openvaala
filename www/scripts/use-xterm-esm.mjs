// xterm 的两个包只有 UMD 的 main，Node 读不出它们的具名导出；单元测试里把包名改指到包里的 ESM 文件（打包走包里的 module 字段，不需要它）
import { registerHooks } from "node:module"

const esmEntries = {
  "@xterm/xterm": "../node_modules/@xterm/xterm/lib/xterm.mjs",
  "@xterm/addon-fit": "../node_modules/@xterm/addon-fit/lib/addon-fit.mjs",
}

registerHooks({
  resolve(specifier, context, nextResolve) {
    // Vite 的 ?url 资源在 Node 行为测试中也以 URL 导出。
    if (specifier.endsWith(".svg?url")) {
      const url = nextResolve(specifier.slice(0, -4), context).url
      return {
        url:
          "data:text/javascript," +
          encodeURIComponent("export default " + JSON.stringify(url)),
        shortCircuit: true,
      }
    }
    const entry = esmEntries[specifier]
    if (!entry) return nextResolve(specifier, context)
    return nextResolve(new URL(entry, import.meta.url).href, context)
  },
})
