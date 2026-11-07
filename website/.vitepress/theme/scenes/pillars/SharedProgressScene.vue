<script setup lang="ts">
// shared-progress：几个窗口显示同一个对话：光标 "you" 在终端发出消息，所有窗口同步流式输出、
// 工具调用从运行中变为完成；光标再移到浏览器接着发第二条消息，所有窗口又同步继续。
// 宽版：终端、Web、桌面端三个主窗口，下面是手机 Web、Zed（ACP）和 Codex（app-server）三个小面板。
// 窄版：终端和 Web 两个主窗口，下面是同样的三个小面板。两版共用同一张时间表，只有 geometry 里的坐标不同。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoCursor from "../primitives/IsoCursor.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"
import type { Vec2 } from "../primitives/iso"

const props = withDefaults(defineProps<{ layout?: "wide" | "narrow" }>(), { layout: "wide" })

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

// 对话的八行，所有窗口共用同一张时间表：at 是出现时刻，width 是占内容区宽度的比例。
const conversationRows = [
  { kind: "user", at: 0.08, width: 0.5 },
  { kind: "text", at: 0.13, width: 0.92 },
  { kind: "text", at: 0.18, width: 0.74 },
  { kind: "tool", at: 0.24, width: 0.56 },
  { kind: "text", at: 0.42, width: 0.86 },
  { kind: "user", at: 0.575, width: 0.42 },
  { kind: "text", at: 0.615, width: 0.9 },
  { kind: "text", at: 0.655, width: 0.62 },
] as const

const toolDoneAt = 0.4
const resetAt = 0.95

interface Frame {
  x: number
  y: number
  width: number
  height: number
}

interface ConversationArea {
  key: string
  x: number
  y: number
  width: number
  rowGap: number
  // isPromptStyle 为 true 时用户消息靠左并带 › 前缀（终端、Codex），否则靠右成气泡。
  isPromptStyle: boolean
}

type MainWindowKey = "terminal" | "web" | "desktop"

// SharedProgressGeometry 是一版布局的全部坐标：主窗口、小面板和光标点击位置。
interface SharedProgressGeometry {
  viewBox: string
  mainWindows: (Frame & { key: MainWindowKey })[]
  phone: Frame
  // zed.dividerX 是代码区和 Agent 面板之间的竖线，codeBarWidths 是代码区每行的宽度。
  zed: Frame & { dividerX: number; codeBarWidths: number[] }
  codex: Frame
  // cursorStart 是光标出现的位置，terminalClick、webClick 是两次点击输入框的位置。
  cursorStart: Vec2
  terminalClick: Vec2
  webClick: Vec2
}

const zedCodeBarWidths = [52, 70, 40, 62, 34, 56, 44]

const wideGeometry: SharedProgressGeometry = {
  viewBox: "0 0 440 304",
  mainWindows: (["terminal", "web", "desktop"] as const).map((key, index) => ({ key, x: 8 + index * 146, y: 20, width: 132, height: 166 })),
  phone: { x: 8, y: 204, width: 42, height: 80 },
  zed: { x: 64, y: 204, width: 174, height: 80, dividerX: 164, codeBarWidths: zedCodeBarWidths },
  codex: { x: 254, y: 204, width: 178, height: 80 },
  cursorStart: [230, 262],
  terminalClick: [104, 170],
  webClick: [252, 170],
}

const narrowGeometry: SharedProgressGeometry = {
  viewBox: "0 0 320 294",
  mainWindows: (["terminal", "web"] as const).map((key, index) => ({ key, x: 4 + index * 160, y: 8, width: 152, height: 166 })),
  phone: { x: 4, y: 192, width: 44, height: 80 },
  zed: { x: 58, y: 192, width: 124, height: 80, dividerX: 120, codeBarWidths: zedCodeBarWidths.map((width) => Math.round(width * 0.6)) },
  codex: { x: 192, y: 192, width: 124, height: 80 },
  cursorStart: [150, 250],
  terminalClick: [118, 159],
  webClick: [278, 159],
}

const geometry = computed(() => (props.layout === "narrow" ? narrowGeometry : wideGeometry))

const windowTitles = computed<Record<MainWindowKey, string>>(() => ({
  terminal: isZh.value ? "终端" : "terminal",
  web: "web",
  desktop: isZh.value ? "桌面端" : "desktop",
}))

// 主窗口里：标题栏下 42 放对话行，底部 22 放输入框。
function getInputFrame(window: Frame): Frame {
  return { x: window.x + 10, y: window.y + window.height - 22, width: window.width - 20, height: 14 }
}

const terminalInput = computed(() => getInputFrame(geometry.value.mainWindows[0]))
const webInput = computed(() => getInputFrame(geometry.value.mainWindows[1]))

const areas = computed<ConversationArea[]>(() => {
  const { mainWindows, phone, zed, codex } = geometry.value
  return [
    ...mainWindows.map((window) => ({
      key: window.key,
      x: window.x + 12,
      y: window.y + 42,
      width: window.width - 24,
      rowGap: 12.5,
      isPromptStyle: window.key === "terminal",
    })),
    { key: "phone", x: phone.x + 6, y: phone.y + 14, width: phone.width - 12, rowGap: 6.2, isPromptStyle: false },
    { key: "zed", x: zed.dividerX + 8, y: zed.y + 12, width: zed.x + zed.width - zed.dividerX - 16, rowGap: 7.6, isPromptStyle: false },
    { key: "codex", x: codex.x + 12, y: codex.y + 12, width: codex.width - 22, rowGap: 7.6, isPromptStyle: true },
  ]
})

const panelLabels = computed(() => {
  const { phone, zed, codex } = geometry.value
  return [
    { text: isZh.value ? "手机" : "mobile", frame: phone },
    { text: "Zed · ACP", frame: zed },
    { text: "Codex · app-server", frame: codex },
  ]
})

function getGrowStops(at: number): SceneStop[] {
  return [
    [0, { opacity: 0, transform: "scaleX(0)" }],
    [at - 0.001, { opacity: 0, transform: "scaleX(0)" }],
    [at, { opacity: 1, transform: "scaleX(0)", easing: "cubic-bezier(0.3, 0.6, 0.4, 1)" }],
    [at + 0.05, { opacity: 1, transform: "scaleX(1)" }],
    [resetAt, { opacity: 1, transform: "scaleX(1)" }],
    [resetAt + 0.03, { opacity: 0, transform: "scaleX(1)" }],
  ]
}

function getRowRect(area: ConversationArea, rowIndex: number) {
  const row = conversationRows[rowIndex]
  const y = area.y + rowIndex * area.rowGap
  const isUser = row.kind === "user"
  const promptOffset = area.isPromptStyle ? 7 : 0
  const width = (area.width - promptOffset) * row.width
  const x = isUser && !area.isPromptStyle ? area.x + area.width - width : area.x + promptOffset
  return { x, y, width, height: isUser && !area.isPromptStyle ? 5 : 3 }
}

// 光标：从画面下方移到终端输入框点击，之后移到 Web 输入框再点击，然后淡出。
const cursorStops = computed<SceneStop[]>(() => {
  const { cursorStart, terminalClick, webClick } = geometry.value
  const start = `translate(${cursorStart[0]}px, ${cursorStart[1]}px)`
  const terminal = `translate(${terminalClick[0]}px, ${terminalClick[1]}px)`
  const web = `translate(${webClick[0]}px, ${webClick[1]}px)`
  return [
    [0, { opacity: 0, transform: start }],
    [0.015, { opacity: 1, transform: start }],
    [0.06, { opacity: 1, transform: terminal }],
    [0.068, { opacity: 1, transform: `${terminal} scale(0.86)` }],
    [0.078, { opacity: 1, transform: terminal }],
    [0.46, { opacity: 1, transform: terminal }],
    [0.54, { opacity: 1, transform: web }],
    [0.552, { opacity: 1, transform: `${web} scale(0.86)` }],
    [0.564, { opacity: 1, transform: web }],
    [0.62, { opacity: 1, transform: web }],
    [0.66, { opacity: 0, transform: web }],
  ]
})

const captions = computed(() => [
  { text: isZh.value ? "在终端开始" : "start in the terminal", stops: getCaptionStops(0, 0.46) },
  { text: isZh.value ? "到浏览器接着做" : "continue in the browser", stops: getCaptionStops(0.46, 0.7) },
  { text: isZh.value ? "每个客户端看到同一份进度" : "every client shows the same progress", stops: getCaptionStops(0.7, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" :view-box="geometry.viewBox" :period="13000" :label="isZh ? '多个入口共享同一个对话' : 'Many clients, one live conversation'">
    <!-- 主窗口：标题栏、输入框 -->
    <g v-for="window in geometry.mainWindows" :key="window.key">
      <rect class="frame" :x="window.x" :y="window.y" :width="window.width" :height="window.height" rx="9" />
      <line class="frame-rule" :x1="window.x" :y1="window.y + 22" :x2="window.x + window.width" :y2="window.y + 22" />
      <text class="scene-label scene-label--small" :x="window.x + 12" :y="window.y + 14">{{ windowTitles[window.key] }}</text>
      <g v-if="window.key === 'desktop'">
        <circle v-for="dot in 3" :key="dot" class="frame-dot" :cx="window.x + window.width - 12 - (dot - 1) * 8" :cy="window.y + 11" r="2" />
      </g>
      <rect v-if="window.key === 'web'" class="frame-address" :x="window.x + 48" :y="window.y + 6" :width="window.width - 60" height="10" rx="5" />
      <rect class="frame-input" v-bind="getInputFrame(window)" rx="7" />
    </g>
    <rect v-track="getShowStops(0.06, 0.1, 0.01)" class="frame-input frame-input--active" v-bind="terminalInput" rx="7" />
    <rect v-track="getShowStops(0.54, 0.6, 0.01)" class="frame-input frame-input--active" v-bind="webInput" rx="7" />

    <!-- 三个小面板：手机 Web、Zed 编辑器、Codex -->
    <rect class="frame" v-bind="geometry.phone" rx="8" />
    <rect class="frame-notch" :x="geometry.phone.x + geometry.phone.width / 2 - 7" :y="geometry.phone.y + 4" width="14" height="3" rx="1.5" />
    <rect class="frame" :x="geometry.zed.x" :y="geometry.zed.y" :width="geometry.zed.width" :height="geometry.zed.height" rx="7" />
    <line class="frame-rule" :x1="geometry.zed.dividerX" :y1="geometry.zed.y" :x2="geometry.zed.dividerX" :y2="geometry.zed.y + geometry.zed.height" />
    <rect
      v-for="(width, line) in geometry.zed.codeBarWidths"
      :key="`code-${line}`"
      class="code-bar"
      :x="geometry.zed.x + 10"
      :y="geometry.zed.y + 13 + line * 9"
      :width="width"
      height="2.5"
      rx="1.25"
    />
    <rect class="frame" v-bind="geometry.codex" rx="7" />
    <text
      v-for="label in panelLabels"
      :key="label.text"
      class="scene-label scene-label--small"
      :x="label.frame.x + label.frame.width / 2"
      :y="label.frame.y + label.frame.height + 14"
      text-anchor="middle"
    >
      {{ label.text }}
    </text>

    <!-- 同一张时间表驱动所有窗口里的对话行 -->
    <g v-for="area in areas" :key="area.key">
      <template v-for="(row, rowIndex) in conversationRows" :key="rowIndex">
        <text
          v-if="row.kind === 'user' && area.isPromptStyle"
          v-track="getShowStops(row.at, resetAt)"
          class="prompt-mark"
          :x="area.x"
          :y="getRowRect(area, rowIndex).y + 3.2"
        >
          ›
        </text>
        <rect
          v-track="row.kind === 'user' ? getShowStops(row.at, resetAt) : getGrowStops(row.at)"
          class="row-bar"
          :class="`row-bar--${row.kind}`"
          v-bind="getRowRect(area, rowIndex)"
          rx="1.5"
        />
        <template v-if="row.kind === 'tool'">
          <circle
            v-track="getShowStops(row.at, toolDoneAt, 0.01)"
            class="tool-running"
            :cx="area.x + area.width - 2"
            :cy="getRowRect(area, rowIndex).y + 1.5"
            r="2"
          />
          <circle
            v-track="getShowStops(toolDoneAt, resetAt, 0.01)"
            class="tool-done"
            :cx="area.x + area.width - 2"
            :cy="getRowRect(area, rowIndex).y + 1.5"
            r="2"
          />
        </template>
      </template>
    </g>

    <g v-track="cursorStops" class="cursor-track">
      <IsoCursor :point="[0, 0]" label="you" />
    </g>

    <template #caption>
      <p
        v-for="(caption, index) in captions"
        :key="caption.text"
        v-track="caption.stops"
        class="scene-caption"
        :style="{ opacity: index === captions.length - 1 ? 1 : 0 }"
      >
        {{ caption.text }}
      </p>
    </template>
  </SceneCanvas>
</template>

<style scoped>
.frame {
  fill: var(--scene-plate);
  stroke: var(--scene-edge);
  stroke-width: 1;
}

.frame-rule {
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.frame-dot {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.frame-address,
.frame-input {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.frame-input--active {
  stroke: var(--scene-hi);
  opacity: 0;
}

.frame-notch {
  fill: var(--scene-lo);
}

.code-bar {
  fill: var(--scene-lo);
}

.row-bar {
  fill: var(--scene-mid);
  transform-box: fill-box;
  transform-origin: left center;
}

.row-bar--user {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1;
}

.row-bar--text {
  fill: var(--scene-lo);
}

.prompt-mark {
  fill: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 7px;
}

.tool-running {
  fill: var(--scene-hi);
  opacity: 0;
}

.tool-done {
  fill: var(--scene-mid);
}

.cursor-track {
  opacity: 0;
  transform-box: view-box;
  transform-origin: 0 0;
}
</style>
