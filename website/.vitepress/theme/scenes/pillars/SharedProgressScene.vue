<script setup lang="ts">
// shared-progress：终端、Web、桌面端三个主窗口，加上手机 Web、Zed（ACP）和 Codex（app-server）三个小面板，
// 显示同一个对话：光标 "you" 在终端发出消息，所有窗口同步流式输出、工具调用从运行中变为完成；
// 光标再移到浏览器接着发第二条消息，所有窗口又同步继续。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoCursor from "../primitives/IsoCursor.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getCaptionStops, getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

// 对话的八行，所有窗口共用同一张时间表：at 是出现时刻，width 是占内容区宽度的比例。
const conversationRows = [
  { kind: "user", at: 0.08, width: 0.5 },
  { kind: "text", at: 0.13, width: 0.92 },
  { kind: "text", at: 0.18, width: 0.74 },
  { kind: "tool", at: 0.24, width: 0.56 },
  { kind: "text", at: 0.42, width: 0.86 },
  { kind: "user", at: 0.6, width: 0.42 },
  { kind: "text", at: 0.66, width: 0.9 },
  { kind: "text", at: 0.72, width: 0.62 },
] as const

const toolDoneAt = 0.4
const resetAt = 0.95

interface ConversationArea {
  key: string
  x: number
  y: number
  width: number
  rowGap: number
  rowCount: number
  // isPromptStyle 为 true 时用户消息靠左并带 › 前缀（终端、Codex），否则靠右成气泡。
  isPromptStyle: boolean
}

// 三个主窗口和三个小面板的位置；area 是窗口里放对话行的区域。
const mainWindows = computed(() => [
  { key: "terminal", x: 8, title: isZh.value ? "终端" : "terminal", isPromptStyle: true },
  { key: "web", x: 154, title: "web", isPromptStyle: false },
  { key: "desktop", x: 300, title: isZh.value ? "桌面端" : "desktop", isPromptStyle: false },
])

const areas = computed<ConversationArea[]>(() => [
  ...mainWindows.value.map((window) => ({
    key: window.key,
    x: window.x + 12,
    y: 62,
    width: 108,
    rowGap: 12.5,
    rowCount: 8,
    isPromptStyle: window.isPromptStyle,
  })),
  { key: "phone", x: 14, y: 218, width: 30, rowGap: 6.2, rowCount: 8, isPromptStyle: false },
  { key: "zed", x: 172, y: 216, width: 58, rowGap: 7.6, rowCount: 8, isPromptStyle: false },
  { key: "codex", x: 266, y: 216, width: 156, rowGap: 7.6, rowCount: 8, isPromptStyle: true },
])

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
const cursorStops: SceneStop[] = [
  [0, { opacity: 0, transform: "translate(230px, 262px)" }],
  [0.015, { opacity: 1, transform: "translate(230px, 262px)" }],
  [0.06, { opacity: 1, transform: "translate(104px, 170px)" }],
  [0.068, { opacity: 1, transform: "translate(104px, 170px) scale(0.86)" }],
  [0.078, { opacity: 1, transform: "translate(104px, 170px)" }],
  [0.46, { opacity: 1, transform: "translate(104px, 170px)" }],
  [0.54, { opacity: 1, transform: "translate(252px, 170px)" }],
  [0.552, { opacity: 1, transform: "translate(252px, 170px) scale(0.86)" }],
  [0.564, { opacity: 1, transform: "translate(252px, 170px)" }],
  [0.62, { opacity: 1, transform: "translate(252px, 170px)" }],
  [0.66, { opacity: 0, transform: "translate(252px, 170px)" }],
]

const captions = computed(() => [
  { text: isZh.value ? "在终端开始" : "start in the terminal", stops: getCaptionStops(0, 0.46) },
  { text: isZh.value ? "到浏览器接着做" : "continue in the browser", stops: getCaptionStops(0.46, 0.87) },
  { text: isZh.value ? "每个客户端看到同一份进度" : "every client shows the same progress", stops: getCaptionStops(0.87, 0.985) },
])
</script>

<template>
  <SceneCanvas class="scene-canvas--flow" view-box="0 0 440 304" :period="13000" :label="isZh ? '多个入口共享同一个对话' : 'Many clients, one live conversation'">
    <!-- 三个主窗口：标题栏、输入框 -->
    <g v-for="window in mainWindows" :key="window.key">
      <rect class="frame" :x="window.x" y="20" width="132" height="166" rx="9" />
      <line class="frame-rule" :x1="window.x" y1="42" :x2="window.x + 132" y2="42" />
      <text class="scene-label scene-label--small" :x="window.x + 12" y="34">{{ window.title }}</text>
      <g v-if="window.key === 'desktop'">
        <circle v-for="dot in 3" :key="dot" class="frame-dot" :cx="window.x + 132 - 12 - (dot - 1) * 8" cy="31" r="2" />
      </g>
      <rect v-if="window.key === 'web'" class="frame-address" :x="window.x + 48" y="26" width="72" height="10" rx="5" />
      <rect class="frame-input" :x="window.x + 10" y="164" width="112" height="14" rx="7" />
    </g>
    <rect v-track="getShowStops(0.06, 0.1, 0.01)" class="frame-input frame-input--active" x="18" y="164" width="112" height="14" rx="7" />
    <rect v-track="getShowStops(0.54, 0.6, 0.01)" class="frame-input frame-input--active" x="164" y="164" width="112" height="14" rx="7" />

    <!-- 三个小面板：手机 Web、Zed 编辑器、Codex -->
    <rect class="frame" x="8" y="204" width="42" height="80" rx="8" />
    <rect class="frame-notch" x="22" y="208" width="14" height="3" rx="1.5" />
    <rect class="frame" x="64" y="204" width="174" height="80" rx="7" />
    <line class="frame-rule" x1="164" y1="204" x2="164" y2="284" />
    <rect v-for="line in 7" :key="`code-${line}`" class="code-bar" x="74" :y="208 + line * 9" :width="[52, 70, 40, 62, 34, 56, 44][line - 1]" height="2.5" rx="1.25" />
    <rect class="frame" x="254" y="204" width="178" height="80" rx="7" />
    <text class="scene-label scene-label--small" x="29" y="298" text-anchor="middle">{{ isZh ? "手机" : "mobile" }}</text>
    <text class="scene-label scene-label--small" x="151" y="298" text-anchor="middle">Zed · ACP</text>
    <text class="scene-label scene-label--small" x="343" y="298" text-anchor="middle">Codex · app-server</text>

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
