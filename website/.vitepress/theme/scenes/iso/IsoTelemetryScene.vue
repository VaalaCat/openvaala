<script setup lang="ts">
// iso-telemetry：中间立着一块指标屏，信号蓝的折线轻轻浮现；
// 屏前左右各平放一张卡片，左边是追踪的瀑布条、右边是日志行，依次出现，表示三类数据一起导出。
import IsoCable from "../primitives/IsoCable.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getRoundedPolylinePath, type Vec3 } from "../primitives/iso"
import { getRiseStops, vTrack, type SceneStop } from "../primitives/motion"

// 指标屏竖在 x-z 面上，可见面朝 +y；facePoint 把屏幕面上的 (a, b) 换成世界坐标。
const screenAt: Vec3 = [-62, -6, 12]
const screenSize: Vec3 = [124, 70, 6]

function facePoint(a: number, b: number): Vec3 {
  return [screenAt[0] + a, screenAt[1] + screenSize[2], screenAt[2] + b]
}

const gridLines = [20, 38, 56].map((b) => [facePoint(12, b), facePoint(screenSize[0] - 12, b)])
const chartValues = [16, 28, 22, 40, 33, 48, 42, 58]
const chartPath = getRoundedPolylinePath(
  chartValues.map((value, index) => facePoint(14 + index * 13.7, value)),
  3,
)

const chartStops: SceneStop[] = [
  [0.08, { transform: "translateY(4px)", opacity: 0, easing: "cubic-bezier(0.4, 0, 0.2, 1)" }],
  [0.5, { transform: "translateY(0px)", opacity: 1 }],
  [0.9, { transform: "translateY(0px)", opacity: 1 }],
  [0.96, { transform: "translateY(0px)", opacity: 0 }],
]

// 两张卡片和卡片上的行；行在卡片顶面（z = 4）上沿 x 方向排列。
const cardSize: Vec3 = [76, 58, 4]
const cards = [
  { at: [-30, 62, 0] as Vec3, rows: [[8, 12, 36], [18, 24, 30], [28, 36, 22], [12, 48, 42]] },
  { at: [62, -12, 0] as Vec3, rows: [[10, 12, 48], [10, 24, 34], [10, 36, 54], [10, 48, 26]] },
]

function getRowPath(cardAt: Vec3, [a, b, length]: number[]): string {
  return getRoundedPolylinePath(
    [
      [cardAt[0] + a, cardAt[1] + b, cardSize[2]],
      [cardAt[0] + a + length, cardAt[1] + b, cardSize[2]],
    ],
    0,
  )
}
</script>

<template>
  <SceneCanvas view-box="-170 -120 340 216" :period="10000">
    <!-- 底座、立柱和指标屏 -->
    <IsoSlab :at="[-28, -18, 0]" :size="[56, 26, 3]" :radius="7" />
    <IsoSlab :at="[-5, -5, 3]" :size="[10, 4, 9]" :radius="2" />
    <IsoSlab :at="screenAt" :size="screenSize" plane="wall-x" :radius="8" :insets="[[6, 6, screenSize[0] - 12, screenSize[1] - 12, 4]]" />
    <IsoCable v-for="(line, index) in gridLines" :key="index" :points="line" tone="lo" :radius="0" />
    <path v-track="chartStops" class="telemetry-line" :d="chartPath" />

    <!-- 屏前两张卡片：追踪瀑布条与日志行，依次出现 -->
    <template v-for="(card, cardIndex) in cards" :key="cardIndex">
      <IsoSlab :at="card.at" :size="cardSize" :radius="8" />
      <path
        v-for="(row, rowIndex) in card.rows"
        :key="rowIndex"
        v-track="getRiseStops(0.14 + cardIndex * 0.05 + rowIndex * 0.1, 0.9, 3)"
        class="iso-row"
        :d="getRowPath(card.at, row)"
      />
    </template>
  </SceneCanvas>
</template>

<style scoped>
.telemetry-line {
  fill: none;
  stroke: var(--scene-hi);
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}
</style>
