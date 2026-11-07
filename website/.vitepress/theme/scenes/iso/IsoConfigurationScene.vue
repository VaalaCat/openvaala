<script setup lang="ts">
// iso-configuration：一叠配置文件，最上面一张摊开成设置表，每行左边是文字、右边是开关；
// 信号蓝的开关来回拨动一次，另一个墨色开关稍后跟着拨动，表示改配置后重新生效。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoTranslate, type Vec3 } from "../primitives/iso"
import { vTrack, type SceneStop } from "../primitives/motion"

// 设置表的位置和尺寸；行在 b 方向等距排列，开关槽在每行右端。
const sheetAt: Vec3 = [-96, -64, 0]
const sheetSize: Vec3 = [192, 128, 4]
const rowB = [22, 50, 78, 106]
const rowLengths = [64, 48, 72, 40]
const trackA = 140
const knobTravel = 18

// 每个开关的旋钮：isOn 是静态画面里的状态，flipAt 为负表示不动。
const knobs = [
  { b: rowB[0], isOn: true, flipAt: -1, isLive: false },
  { b: rowB[1], isOn: true, flipAt: 0.18, isLive: true },
  { b: rowB[2], isOn: false, flipAt: -1, isLive: false },
  { b: rowB[3], isOn: true, flipAt: 0.5, isLive: false },
]

function getKnobAt(b: number, isOn: boolean): Vec3 {
  return [sheetAt[0] + trackA + 3 + (isOn ? knobTravel : 0), sheetAt[1] + b - 6, sheetSize[2]]
}

// getFlipStops 让旋钮从"关"拨到"开"并停住，循环末尾悄悄拨回。
function getFlipStops(flipAt: number): SceneStop[] {
  if (flipAt < 0) return [[0, { transform: "translate(0px, 0px)" }]]
  const off = getIsoTranslate([-knobTravel, 0, 0])
  return [
    [flipAt, { transform: off }],
    [flipAt + 0.06, { transform: "translate(0px, 0px)" }],
    [0.9, { transform: "translate(0px, 0px)" }],
    [0.97, { transform: off }],
  ]
}
</script>

<template>
  <SceneCanvas view-box="-196 -111 392 196" :period="10000">
    <!-- 后面两张配置文件，只露出边缘 -->
    <IsoSlab :at="[sheetAt[0] - 22, sheetAt[1] - 22, 0]" :size="sheetSize" :radius="12" />
    <IsoSlab :at="[sheetAt[0] - 11, sheetAt[1] - 11, 0]" :size="sheetSize" :radius="12" />

    <!-- 设置表：文字行和开关槽 -->
    <IsoSlab
      :at="sheetAt"
      :size="sheetSize"
      :radius="12"
      :rows="rowB.map((b, index) => [18, b, rowLengths[index]] as [number, number, number])"
      :insets="rowB.map((b) => [trackA, b - 7, 40, 14, 7] as [number, number, number, number, number])"
    />

    <g v-for="knob in knobs" :key="knob.b" v-track="getFlipStops(knob.flipAt)">
      <IsoSlab :at="getKnobAt(knob.b, knob.isOn)" :size="[14, 12, 4]" :radius="6" :tone="knob.isLive ? 'hi' : 'edge'" />
    </g>
  </SceneCanvas>
</template>
