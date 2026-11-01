// 动态 import 页面模块失败时浏览器给出的消息：Chrome / Edge 是「Failed to fetch dynamically imported
// module」，Firefox 是「error loading dynamically imported module」，Safari 是「Importing a module script failed」；
// 页面依赖的样式没加载出来时，Vite 的预加载报「Unable to preload CSS」。
const moduleLoadFailure =
  /dynamically imported module|Importing a module script failed|Unable to preload CSS/

// 页面模块没加载出来（多为面板刚升级、旧资源已下线）；其他渲染错误不是。
export function isModuleLoadError(error: unknown): boolean {
  return error instanceof Error && moduleLoadFailure.test(error.message)
}
