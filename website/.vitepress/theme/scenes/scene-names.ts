// 场景名清单：config.ts 的 Markdown 插件用它判断 <!-- scene: name --> 是否有效，Scene.vue 按它分派组件。

export const sceneNames = [
  // 首页卖点，同时可嵌入正文。
  "unified-runtime",
  "shared-progress",
  "ecosystem",
  "portable-conversations",
  // 各页面顶部的等轴测插图，同时可嵌入正文。
  "iso-quickstart",
  "iso-install",
  "iso-terminal",
  "iso-web",
  "iso-desktop",
  "iso-editors",
  "iso-codex",
  "iso-conversations",
  "iso-configuration",
  "iso-models",
  "iso-agents",
  "iso-executors",
  "iso-tools",
  "iso-extensions",
  "iso-telemetry",
  "iso-cli",
  "iso-architecture",
  // 正文专用的说明场景。
  "conversation-fork",
  "context-compaction",
  "runtime-layers",
  "input-queue",
  "subagents",
  "tool-hooks",
  "executor-routing",
] as const

export type SceneName = (typeof sceneNames)[number]

// narrowLayoutSceneNames 是另画了一版窄屏布局的场景：组件接收 layout 属性，Scene 同时渲染宽、窄两版，按屏幕宽度只显示一版。
export const narrowLayoutSceneNames: readonly SceneName[] = [
  "unified-runtime",
  "shared-progress",
  "ecosystem",
  "portable-conversations",
  "input-queue",
  "subagents",
  "tool-hooks",
  "executor-routing",
]

// isSceneName 判断字符串是否是已登记的场景名？
export function isSceneName(name: string): name is SceneName {
  return (sceneNames as readonly string[]).includes(name)
}

// getPageSceneName 返回页面顶部插图的场景名：取路径最后一段，configuration/overview 对应 iso-configuration。
export function getPageSceneName(relativePath: string): SceneName | undefined {
  const pageKey = relativePath
    .replace(/^zh\//, "")
    .replace(/\.md$/, "")
    .split("/")
    .pop()
  const name = `iso-${pageKey === "overview" ? "configuration" : pageKey}`
  return isSceneName(name) ? name : undefined
}
