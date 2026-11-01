<script setup lang="ts">
// iso-terminal：一面竖立的终端窗口，顶栏三个圆点，正文逐行流式输出，光标跟着最新一行前进；底部是输入框。
// 静态画面是所有行都已输出、光标停在最后一行末尾。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoPrism, getIsoTranslate, type Vec3 } from "../primitives/iso"
import { vTrack, type SceneStop } from "../primitives/motion"

const windowAt: Vec3 = [-120, -8, -30]
const windowSize: Vec3 = [240, 90, 8]
const { getFacePoint } = getIsoPrism("wall-x", windowAt, windowSize, 8)

// 每行在 startAt 开始从左向右写出，用时与长度成正比。
const rowStart = 12
const rows: { b: number; length: number; startAt: number; span: number }[] = []
let cursorAt = 0.06
for (const [b, length] of [
  [64, 112],
  [55, 152],
  [46, 84],
  [37, 134],
] as const) {
  const span = (length / 200) * 0.16
  rows.push({ b, length, startAt: cursorAt, span })
  cursorAt += span + 0.035
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

// 光标的静态位置在最后一行末尾；轨道里每行随写出线性前进，换行时瞬间跳到下一行开头。
const lastRow = rows[rows.length - 1]
const caretHome: [number, number] = [rowStart + lastRow.length + 4, lastRow.b]

function getCaretOffset(a: number, b: number): string {
  const [x1, y1, z1] = getFacePoint(a, b)
  const [x0, y0, z0] = getFacePoint(caretHome[0], caretHome[1])
  return getIsoTranslate([x1 - x0, y1 - y0, z1 - z0])
}

const caretStops: SceneStop[] = [[0, { opacity: 0, transform: getCaretOffset(rowStart, rows[0].b) }]]
rows.forEach((row, index) => {
  const start = getCaretOffset(rowStart + 2, row.b)
  const end = getCaretOffset(rowStart + row.length + 4, row.b)
  caretStops.push([row.startAt - 0.001, { opacity: index === 0 ? 0 : 1, transform: start }])
  caretStops.push([row.startAt, { opacity: 1, transform: start, easing: "linear" }])
  caretStops.push([row.startAt + row.span, { opacity: 1, transform: end }])
  const next = rows[index + 1]
  if (next) caretStops.push([next.startAt - 0.002, { opacity: 1, transform: end }])
})
caretStops.push([0.93, { opacity: 1, transform: getCaretOffset(caretHome[0], caretHome[1]) }])
caretStops.push([0.96, { opacity: 0, transform: getCaretOffset(caretHome[0], caretHome[1]) }])

const caretPath = getFaceRoundedRect(getFacePoint, [caretHome[0], caretHome[1] - 3.5, 3.5, 7, 0.6])
const headerRule = getFaceLine(getFacePoint, [0, 74, windowSize[0]])
</script>

<template>
  <SceneCanvas view-box="-120 -130 240 222" :period="10000">
    <IsoSlab
      :at="windowAt"
      :size="windowSize"
      plane="wall-x"
      :radius="8"
      :insets="[[8, 79, 6, 6, 3], [17, 79, 6, 6, 3], [26, 79, 6, 6, 3], [8, 7, 224, 15, 5]]"
    />
    <path class="scene-wire scene-wire--lo" :d="headerRule" />
    <path class="iso-row prompt-mark" :d="getFaceLine(getFacePoint, [16, 14.5, 4])" />
    <path
      v-for="row in rows"
      :key="row.b"
      v-track="getRowStops(row.startAt, row.span)"
      class="stream-row"
      :d="getFaceLine(getFacePoint, [rowStart, row.b, row.length])"
    />
    <path v-track="caretStops" class="caret" :d="caretPath" />
  </SceneCanvas>
</template>

<style scoped>
.prompt-mark {
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
