<script setup lang="ts">
// iso-models：一块底板上有三个插口，代表三种模型协议；三枚芯片依次从左上方滑入插口，
// 每插好一枚，插口旁的信号蓝指示灯就移到它那里亮一下，芯片左上方同时出现协议名。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoTranslate, project, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const boardAt: Vec3 = [-34, -118, 0]
const boardSize: Vec3 = [76, 236, 10]
const chipSize: Vec3 = [46, 46, 8]
const slideDistance = 60

// 三个协议芯片沿 y 方向排开；dockAt 是芯片插好的时刻。
const chips = [
  { label: "OpenAI Chat", b: 14, dockAt: 0.14 },
  { label: "OpenAI Responses", b: 95, dockAt: 0.3 },
  { label: "Claude Messages", b: 176, dockAt: 0.46 },
]

function getChipAt(b: number): Vec3 {
  return [boardAt[0] + 15, boardAt[1] + b, boardSize[2]]
}

// 标签放在芯片滑入方向（左上）的空白处，不随芯片滑动，插好时才出现。
function getLabelPosition(b: number) {
  const [x, y, z] = getChipAt(b)
  const [sx, sy] = project([x - 8, y + chipSize[1] / 2, z + chipSize[2]])
  return { x: sx - 6, y: sy - 6 }
}

function getSlideStops(dockAt: number): SceneStop[] {
  const away = getIsoTranslate([-slideDistance, 0, 0])
  return [
    [dockAt - 0.09, { opacity: 0, transform: away }],
    [dockAt - 0.06, { opacity: 1, transform: away }],
    [dockAt, { opacity: 1, transform: "translate(0px, 0px)" }],
    [0.9, { opacity: 1, transform: "translate(0px, 0px)" }],
    [0.96, { opacity: 0, transform: "translate(0px, 0px)" }],
  ]
}

// 指示灯：在底板右侧沿 y 排开的三个位置之间跳动，每次在芯片插好时亮起。
const lightPositions = chips.map((chip) => project([boardAt[0] + boardSize[0] - 7, boardAt[1] + chip.b + chipSize[1] / 2, boardSize[2]]))
const lightStops: SceneStop[] = chips.flatMap((chip, index) => {
  const [cx, cy] = lightPositions[index]
  const [fx, fy] = lightPositions[0]
  const transform = `translate(${cx - fx}px, ${cy - fy}px)`
  return [
    [chip.dockAt + 0.005, { opacity: 0, transform }],
    [chip.dockAt + 0.02, { opacity: 1, transform }],
    [chip.dockAt + 0.12, { opacity: 1, transform }],
    [chip.dockAt + 0.15, { opacity: 0, transform }],
  ] as SceneStop[]
})
</script>

<template>
  <SceneCanvas view-box="-211 -112 360 218" :period="10000">
    <IsoSlab
      :at="boardAt"
      :size="boardSize"
      :radius="10"
      :insets="chips.map((chip) => [12, chip.b - 3, 52, 52, 7] as [number, number, number, number, number])"
    />

    <g v-for="chip in chips" :key="chip.label" v-track="getSlideStops(chip.dockAt)">
      <IsoSlab :at="getChipAt(chip.b)" :size="chipSize" :radius="6" :insets="[[10, 10, 26, 26, 4]]" />
    </g>
    <text
      v-for="chip in chips"
      :key="`label-${chip.label}`"
      v-track="getShowStops(chip.dockAt, 0.9, 0.03)"
      class="scene-label scene-label--ink"
      :x="getLabelPosition(chip.b).x"
      :y="getLabelPosition(chip.b).y"
      text-anchor="end"
    >
      {{ chip.label }}
    </text>

    <ellipse
      v-track="lightStops"
      class="scene-light models-light"
      :cx="lightPositions[0][0]"
      :cy="lightPositions[0][1]"
      rx="4.5"
      ry="2.6"
    />
  </SceneCanvas>
</template>
