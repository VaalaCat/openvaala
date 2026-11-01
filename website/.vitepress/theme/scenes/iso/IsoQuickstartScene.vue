<script setup lang="ts">
// iso-quickstart：一块平放的终端，提示符后光标逐键敲出命令；回车后输出逐行出现，
// 新的提示符上光标停住闪烁。静态画面是命令和输出都已出现、光标停在新提示符上。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getFaceLine, getFaceRoundedRect, getIsoPrism, getIsoTranslate, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const deviceAt: Vec3 = [-96, -52, 0]
const deviceSize: Vec3 = [192, 104, 12]
const { getFacePoint } = getIsoPrism("floor", deviceAt, deviceSize, 12)

const commandRow = 30
const promptRow = 82
const textStart = 32
const step = "steps(1, end)"

// 命令按单词画成整条线，每次敲击多露出 7 个单位；光标同时跳到已敲出部分的末尾。
const words: { a: number; length: number; keystrokes: number[] }[] = []
const caretMoves: { at: number; a: number }[] = []
let wordStart = textStart
let typedAt = 0.08
for (const length of [22, 10, 36]) {
  const keystrokes: number[] = []
  for (let typed = 7; typed < length + 7; typed += 7) {
    keystrokes.push(typedAt)
    caretMoves.push({ at: typedAt, a: wordStart + Math.min(typed, length) + 3 })
    typedAt += 0.022
  }
  words.push({ a: wordStart, length, keystrokes })
  wordStart += length + 6
}
const typedEnd = typedAt - 0.022
const enterAt = typedEnd + 0.09

function getWordStops(word: { length: number; keystrokes: number[] }): SceneStop[] {
  const stops: SceneStop[] = [[word.keystrokes[0] - 0.001, { opacity: 0, transform: "scale(0)", easing: step }]]
  word.keystrokes.forEach((at, index) => {
    const shown = Math.min(1, ((index + 1) * 7) / word.length)
    stops.push([at, { opacity: 1, transform: `scale(${shown})`, easing: step }])
  })
  stops.push([0.94, { opacity: 1, transform: "scale(1)" }], [0.97, { opacity: 0, transform: "scale(1)" }])
  return stops
}

const outputRows = [
  { b: 46, length: 124, shownAt: enterAt + 0.04 },
  { b: 57, length: 148, shownAt: enterAt + 0.09 },
  { b: 68, length: 84, shownAt: enterAt + 0.14 },
]
const newPromptAt = enterAt + 0.21

// getCaretOffset 返回光标从静态位置（新提示符后）移到面上 (a, b) 的位移。
function getCaretOffset(a: number, b: number): string {
  const [x1, y1, z1] = getFacePoint(a, b)
  const [x0, y0, z0] = getFacePoint(textStart, promptRow)
  return getIsoTranslate([x1 - x0, y1 - y0, z1 - z0])
}

// 光标轨道：逐键跳动；敲完后闪两下，回车后隐藏，新提示符出现后停住闪烁。
const caretStops: SceneStop[] = [[0, { opacity: 1, transform: getCaretOffset(textStart, commandRow), easing: step }]]
for (const move of caretMoves) caretStops.push([move.at, { opacity: 1, transform: getCaretOffset(move.a, commandRow), easing: step }])
const typedCaret = getCaretOffset(caretMoves[caretMoves.length - 1].a, commandRow)
for (let at = typedEnd + 0.025, index = 0; at < enterAt; at += 0.025, index++) {
  caretStops.push([at, { opacity: index % 2 === 0 ? 0 : 1, transform: typedCaret, easing: step }])
}
caretStops.push([enterAt, { opacity: 0, transform: typedCaret, easing: step }])
for (let at = newPromptAt, index = 0; at < 0.94; at += 0.045, index++) {
  caretStops.push([at, { opacity: index % 2 === 0 ? 1 : 0, transform: getCaretOffset(textStart, promptRow), easing: step }])
}
caretStops.push([0.94, { opacity: 0, transform: getCaretOffset(textStart, promptRow) }])

const caretPath = getFaceRoundedRect(getFacePoint, [textStart, promptRow - 4.5, 5, 9, 0.8])
const promptMark = (b: number) => getFaceLine(getFacePoint, [20, b, 4])
const headerRule = getFaceLine(getFacePoint, [0, 17, deviceSize[0]])
</script>

<template>
  <SceneCanvas view-box="-146 -104 292 200" :period="10000">
    <IsoSlab :at="deviceAt" :size="deviceSize" :radius="12" :insets="[[9, 7, 5, 5, 2.5], [16, 7, 5, 5, 2.5], [23, 7, 5, 5, 2.5]]" />
    <path class="scene-wire scene-wire--lo" :d="headerRule" />

    <path class="iso-row command-row" :d="promptMark(commandRow)" />
    <path
      v-for="word in words"
      :key="word.a"
      v-track="getWordStops(word)"
      class="typed-row"
      :d="getFaceLine(getFacePoint, [word.a, commandRow, word.length])"
    />
    <path
      v-for="row in outputRows"
      :key="row.b"
      v-track="getShowStops(row.shownAt, 0.94, 0.02)"
      class="iso-row"
      :d="getFaceLine(getFacePoint, [20, row.b, row.length])"
    />
    <path v-track="getShowStops(newPromptAt, 0.94, 0.002)" class="iso-row command-row" :d="promptMark(promptRow)" />

    <path v-track="caretStops" class="caret" :d="caretPath" />
  </SceneCanvas>
</template>

<style scoped>
.command-row {
  stroke: var(--scene-mid);
}

.typed-row {
  fill: none;
  stroke: var(--scene-mid);
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
