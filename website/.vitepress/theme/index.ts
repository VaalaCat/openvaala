import type { Theme } from "vitepress"
import "@fontsource-variable/noto-serif"
import "@fontsource/ibm-plex-sans/400.css"
import "@fontsource/ibm-plex-sans/400-italic.css"
import "@fontsource/ibm-plex-sans/500.css"
import "@fontsource/ibm-plex-sans/600.css"
import "@fontsource-variable/jetbrains-mono"
import "@fontsource-variable/jetbrains-mono/wght-italic.css"
import "./styles/base.css"
import "./styles/prose.css"
import "./styles/scenes.css"
import Layout from "./Layout.vue"
import Scene from "./scenes/Scene.vue"

// OpenVaala 文档站主题：不继承 VitePress 默认主题，布局、搜索和样式全部自带。
// Scene 全局注册，供 Markdown 插件把 <!-- scene: name --> 换成的 <Scene> 标签使用。
export default {
  Layout,
  enhanceApp({ app }) {
    app.component("Scene", Scene)
  },
} satisfies Theme
