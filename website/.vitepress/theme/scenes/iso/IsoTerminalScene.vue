<script setup lang="ts">
// iso-terminal：一面朝右下的终端窗口，里面是 OpenVaala 的终端界面：顶部标题线，下面依次是用户消息、
// 工具调用树和回复，底部是输入行和状态栏（模型 · 上下文用量 · 工作目录）。
// 输入行里先敲出一条消息，回车后消息上移到对话里；随后工具调用逐行出现，回复从左向右流式写出，
// 状态栏的上下文用量跟着变长。信号蓝的块状光标留在输入行里闪烁。静态画面是整轮对话都已写完。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoTranslate, getRoundedPolylinePath, type Vec3 } from "../primitives/iso"
import { getRiseStops, vTrack, type SceneStop } from "../primitives/motion"

// 窗口竖在 y-z 面上、正面朝右下；正面中心投影在画布原点。
const windowSize: Vec3 = [196, 112, 6]
const windowAt: Vec3 = [-windowSize[2], -windowSize[0] / 2, -windowSize[1] / 2]

// getWindowPoint 把正面上从左上角量起的 (u, v)（u 向右、v 向下）换成世界坐标。
function getWindowPoint(u: number, v: number): Vec3 {
  return [windowAt[0] + windowSize[2], windowAt[1] + windowSize[0] - u, windowAt[2] + windowSize[1] - v]
}

const line = (u: number, v: number, length: number) => getFaceLine(getWindowPoint, [u, v, length])
const polyline = (points: [number, number][]) => getRoundedPolylinePath(points.map(([u, v]) => getWindowPoint(u, v)), 0)
const rect = (u: number, v: number, width: number, height: number, radius: number) =>
  getFaceRoundedRect(getWindowPoint, [u, v, width, height, radius])

// getOffset 返回正面上从 (u0, v0) 到 (u1, v1) 的屏幕位移。
function getOffset(u0: number, v0: number, u1: number, v1: number): string {
  const [x0, y0, z0] = getWindowPoint(u0, v0)
  const [x1, y1, z1] = getWindowPoint(u1, v1)
  return getIsoTranslate([x1 - x0, y1 - y0, z1 - z0])
}

// 各行在正面上的竖向位置。
const screenV = 15
const titleV = 23
const userV = 32
const toolV = [41, 49, 57]
const replyV = [66, 74, 82]
const inputV = 92
const statusV = 101
const textU = 23

// 时间线：敲字、回车、工具调用、回复三行。
const typeAt = 0.05
const typeSpan = 0.1
const sendAt = typeAt + typeSpan + 0.03
const toolAt = [0.25, 0.31, 0.37]
const replies = [
  { v: replyV[0], length: 162, startAt: 0.44, span: 0.12 },
  { v: replyV[1], length: 140, startAt: 0.57, span: 0.1 },
  { v: replyV[2], length: 92, startAt: 0.68, span: 0.07 },
]
const userLength = 112

// getStreamStops 让一行在 [startAt, startAt + span] 里从左向右写出，循环末尾淡出。
function getStreamStops(startAt: number, span: number, endAt = 0.93): SceneStop[] {
  return [
    [startAt - 0.001, { opacity: 0, transform: "scale(0)" }],
    [startAt, { opacity: 1, transform: "scale(0)", easing: "linear" }],
    [startAt + span, { opacity: 1, transform: "scale(1)" }],
    [endAt, { opacity: 1, transform: "scale(1)" }],
    [endAt + 0.03, { opacity: 0, transform: "scale(1)" }],
  ]
}

// 输入行里敲出的消息：写出后在回车时消失。
const typedStops: SceneStop[] = [
  [typeAt - 0.001, { opacity: 0, transform: "scale(0)" }],
  [typeAt, { opacity: 1, transform: "scale(0)", easing: "linear" }],
  [typeAt + typeSpan, { opacity: 1, transform: "scale(1)" }],
  [sendAt - 0.001, { opacity: 1, transform: "scale(1)" }],
  [sendAt, { opacity: 0, transform: "scale(1)" }],
]

// 光标的静态位置在输入行开头；敲字时跟着消息末尾前进，回车后跳回开头。
const cursorU = 20
const cursorMoveStops: SceneStop[] = [
  [typeAt, { transform: "translate(0px, 0px)", easing: "linear" }],
  [typeAt + typeSpan, { transform: getOffset(cursorU, inputV, cursorU + userLength + 3, inputV) }],
  [sendAt - 0.001, { transform: getOffset(cursorU, inputV, cursorU + userLength + 3, inputV), easing: "steps(1, end)" }],
  [sendAt, { transform: "translate(0px, 0px)" }],
]

// 光标敲字时常亮，其余时间按 1 秒一次闪烁。
const cursorBlinkStops: SceneStop[] = [[0, { opacity: 1, easing: "steps(1, end)" }]]
for (let at = sendAt + 0.035, index = 0; at < 1; at += 0.05, index++) {
  cursorBlinkStops.push([at, { opacity: index % 2 === 0 ? 0 : 1, easing: "steps(1, end)" }])
}

// 状态栏的上下文用量：发出消息、工具调用、回复写完时各涨一截。
const meterU = 68
const meterWidth = 30
const meterStops: SceneStop[] = [
  [0, { transform: "scale(0.2)" }],
  [sendAt, { transform: "scale(0.2)" }],
  [sendAt + 0.04, { transform: "scale(0.35)" }],
  [toolAt[2], { transform: "scale(0.35)" }],
  [toolAt[2] + 0.04, { transform: "scale(0.55)" }],
  [replies[2].startAt + replies[2].span, { transform: "scale(0.55)" }],
  [replies[2].startAt + replies[2].span + 0.04, { transform: "scale(0.8)" }],
  [0.93, { transform: "scale(0.8)" }],
  [0.97, { transform: "scale(0.2)" }],
]

const promptMark = (v: number) => polyline([[14, v - 3], [17, v], [14, v + 3]])
// 工具调用树：第一行是 ✓，下面两行用 ├ 和 └ 连到它，每段连线随自己那一行出现。
const toolMarks = [
  polyline([[13, toolV[0]], [15.5, toolV[0] + 2.5], [19.5, toolV[0] - 3]]),
  polyline([[18, toolV[0] + 5], [18, toolV[1] + 4]]) + polyline([[18, toolV[1]], [22, toolV[1]]]),
  polyline([[18, toolV[1] + 4], [18, toolV[2]], [22, toolV[2]]]),
]
const toolRows = [
  line(textU, toolV[0], 52),
  line(textU + 3, toolV[1], 62),
  line(textU + 3, toolV[2], 44),
]
</script>

<template>
  <SceneCanvas view-box="-100 -110 200 220" :period="10000">
    <IsoSlab :at="windowAt" :size="windowSize" plane="wall-y" :radius="8" />
    <path v-for="u in [11, 18, 25]" :key="u" class="iso-inset" :d="rect(u - 2.2, 6, 4.4, 4.4, 2.2)" />
    <path class="iso-inset" :d="rect(6, screenV, windowSize[0] - 12, windowSize[1] - screenV - 6, 5)" />

    <!-- 标题线：── 标题 ────── -->
    <path class="term-rule" :d="line(14, titleV, 6)" />
    <path class="term-row" :d="line(25, titleV, 54)" />
    <path class="term-rule" :d="line(85, titleV, windowSize[0] - 99)" />

    <!-- 对话：用户消息、工具调用树、回复 -->
    <g v-track="getRiseStops(sendAt, 0.93, 2, 0.01)">
      <path class="term-mark" :d="promptMark(userV)" />
      <path class="term-row term-row--mid" :d="line(textU, userV, userLength)" />
    </g>
    <g v-for="(at, index) in toolAt" :key="at" v-track="getRiseStops(at, 0.93, 2, 0.02)">
      <path class="term-mark" :d="toolMarks[index]" />
      <path class="term-row" :d="toolRows[index]" />
    </g>
    <path v-track="getRiseStops(replies[0].startAt, 0.93, 0, 0.01)" class="term-bullet" :d="rect(13.5, replyV[0] - 1.6, 3.2, 3.2, 1.6)" />
    <path
      v-for="reply in replies"
      :key="reply.v"
      v-track="getStreamStops(reply.startAt, reply.span)"
      class="term-row term-row--mid term-row--stream"
      :d="line(textU, reply.v, reply.length)"
    />

    <!-- 输入行：提示符、正在敲的消息和块状光标 -->
    <path class="term-mark" :d="promptMark(inputV)" />
    <path v-track="typedStops" class="term-row term-row--mid term-row--stream" :d="line(cursorU, inputV, userLength)" />
    <g v-track="cursorMoveStops">
      <path v-track="cursorBlinkStops" class="term-cursor" :d="rect(cursorU, inputV - 3.5, 5, 7, 0.6)" />
    </g>

    <!-- 状态栏：模型 · 上下文用量 · 工作目录 -->
    <path class="term-row term-row--mid" :d="line(14, statusV, 42)" />
    <path class="term-bullet" :d="rect(60.5, statusV - 1, 2, 2, 1)" />
    <path class="term-meter" :d="rect(meterU, statusV - 2.5, meterWidth, 5, 2.5)" />
    <path v-track="meterStops" class="term-row term-row--mid term-row--stream" :d="line(meterU + 2.5, statusV, meterWidth - 5)" />
    <path class="term-bullet" :d="rect(103.5, statusV - 1, 2, 2, 1)" />
    <path class="term-row" :d="line(111, statusV, 56)" />
  </SceneCanvas>
</template>

<style scoped>
.term-rule {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 1;
}

.term-row {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 2;
  stroke-linecap: round;
}

.term-row--mid {
  stroke: var(--scene-mid);
}

/* 流式写出的行从左端开始变长；窗口朝右下，行的左端在外框的左下角。 */
.term-row--stream {
  transform-box: fill-box;
  transform-origin: 0 100%;
}

.term-mark {
  fill: none;
  stroke: var(--scene-mid);
  stroke-width: 1.2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.term-bullet {
  fill: var(--scene-mid);
  stroke: none;
}

.term-meter {
  fill: none;
  stroke: var(--scene-mid);
}

.term-cursor {
  fill: var(--scene-hi);
  stroke: none;
}
</style>
