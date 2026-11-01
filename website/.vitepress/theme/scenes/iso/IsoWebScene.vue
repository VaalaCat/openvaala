<script setup lang="ts">
// iso-web：平放的浏览器卡片和旁边的手机，同一个对话在两边同步流式输出：每一行在两块屏幕上同时写出，
// 光标只在浏览器上跟着最新一行前进。静态画面是两边都已写完。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoPrism, getIsoTranslate, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const browserAt: Vec3 = [-110, -80, 0]
const browserSize: Vec3 = [170, 124, 6]
const phoneAt: Vec3 = [86, 0, 0]
const phoneSize: Vec3 = [40, 80, 7]
const browserFace = getIsoPrism("floor", browserAt, browserSize, 10).getFacePoint
const phoneFace = getIsoPrism("floor", phoneAt, phoneSize, 9).getFacePoint

// 回复逐行写出：浏览器与手机上的同一行同时开始、同时写完，手机行按屏宽缩短。
const replyRows: { browserB: number; browserLength: number; phoneB: number; phoneLength: number; startAt: number; span: number }[] = []
let cursorAt = 0.16
for (const [browserLength, phoneLength] of [
  [132, 26],
  [100, 20],
  [146, 28],
  [72, 14],
] as const) {
  const index = replyRows.length
  const span = (browserLength / 150) * 0.11
  replyRows.push({ browserB: 52 + index * 11, browserLength, phoneB: 28 + index * 8, phoneLength, startAt: cursorAt, span })
  cursorAt += span + 0.03
}

function getRowStops(startAt: number, span: number): SceneStop[] {
  return [
    [startAt - 0.001, { opacity: 0, transform: "scale(0)" }],
    [startAt, { opacity: 1, transform: "scale(0)", easing: "linear" }],
    [startAt + span, { opacity: 1, transform: "scale(1)" }],
    [0.93, { opacity: 1, transform: "scale(1)" }],
    [0.96, { opacity: 0, transform: "scale(1)" }],
  ]
}

const rowStart = 14
const lastRow = replyRows[replyRows.length - 1]
const caretHome: [number, number] = [rowStart + lastRow.browserLength + 4, lastRow.browserB]

function getCaretOffset(a: number, b: number): string {
  const [x1, y1, z1] = browserFace(a, b)
  const [x0, y0, z0] = browserFace(caretHome[0], caretHome[1])
  return getIsoTranslate([x1 - x0, y1 - y0, z1 - z0])
}

const caretStops: SceneStop[] = [[0, { opacity: 0, transform: getCaretOffset(rowStart, replyRows[0].browserB) }]]
replyRows.forEach((row, index) => {
  const start = getCaretOffset(rowStart + 2, row.browserB)
  const end = getCaretOffset(rowStart + row.browserLength + 4, row.browserB)
  caretStops.push([row.startAt - 0.001, { opacity: index === 0 ? 0 : 1, transform: start }])
  caretStops.push([row.startAt, { opacity: 1, transform: start, easing: "linear" }])
  caretStops.push([row.startAt + row.span, { opacity: 1, transform: end }])
  const next = replyRows[index + 1]
  if (next) caretStops.push([next.startAt - 0.002, { opacity: 1, transform: end }])
})
caretStops.push([0.93, { opacity: 1, transform: getCaretOffset(caretHome[0], caretHome[1]) }])
caretStops.push([0.96, { opacity: 0, transform: getCaretOffset(caretHome[0], caretHome[1]) }])

const caretPath = getFaceRoundedRect(browserFace, [caretHome[0], caretHome[1] - 4, 5, 8, 0.8])
const userBubbles = [
  { path: getFaceRoundedRect(browserFace, [92, 26, 64, 14, 6]) },
  { path: getFaceRoundedRect(phoneFace, [14, 12, 20, 8, 3]) },
]
</script>

<template>
  <SceneCanvas view-box="-146 -112 292 222" :period="10000">
    <IsoSlab
      :at="browserAt"
      :size="browserSize"
      :radius="10"
      :insets="[[8, 9, 5, 5, 2.5], [15, 9, 5, 5, 2.5], [22, 9, 5, 5, 2.5], [32, 7, 128, 9, 4.5]]"
    />
    <IsoSlab :at="phoneAt" :size="phoneSize" :radius="9" :insets="[[4, 5, 32, 70, 6]]" />

    <path v-for="bubble in userBubbles" :key="bubble.path" v-track="getShowStops(0.06, 0.93, 0.03)" class="user-bubble" :d="bubble.path" />
    <template v-for="row in replyRows" :key="row.browserB">
      <path
        v-track="getRowStops(row.startAt, row.span)"
        class="stream-row"
        :d="getFaceLine(browserFace, [rowStart, row.browserB, row.browserLength])"
      />
      <path
        v-track="getRowStops(row.startAt, row.span)"
        class="stream-row"
        :d="getFaceLine(phoneFace, [8, row.phoneB, row.phoneLength])"
      />
    </template>
    <path v-track="caretStops" class="caret" :d="caretPath" />
  </SceneCanvas>
</template>

<style scoped>
.user-bubble {
  fill: none;
  stroke: var(--scene-mid);
}

.stream-row {
  fill: none;
  stroke: var(--scene-lo);
  stroke-width: 2;
  stroke-linecap: round;
  transform-box: fill-box;
  transform-origin: 0 0;
}

.caret {
  fill: var(--scene-hi);
  stroke: none;
}
</style>
