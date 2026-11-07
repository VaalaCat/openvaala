<script setup lang="ts">
// iso-agents：后方是根 Agent 的卡片，三张较小的子 Agent 卡片依次从它身上滑出、落到前方，
// 停稳后才由细线连回根卡片，细线也先于卡片淡出；稍后中间那张子 Agent 完成任务，一个信号蓝数据包沿连线汇报回根卡片。
import IsoCable from "../primitives/IsoCable.vue"
import IsoPacket from "../primitives/IsoPacket.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoTranslate, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const rootCenter: [number, number] = [-34, -34]
const rootSize: Vec3 = [84, 64, 8]
const subagentSize: Vec3 = [50, 38, 5]

// 子 Agent 卡片的中心位置和出现时刻，从后往前排列，便于按画家顺序绘制。
const subagents = [
  { center: [70, -54] as [number, number], spawnAt: 0.06 },
  { center: [-58, 74] as [number, number], spawnAt: 0.18 },
  { center: [52, 52] as [number, number], spawnAt: 0.3 },
]

function getCardAt(center: [number, number], size: Vec3, z = 0): Vec3 {
  return [center[0] - size[0] / 2, center[1] - size[1] / 2, z]
}

// getCable 返回根卡片中心到子卡片中心连线上 [from, to] 这一段。
function getCable(center: [number, number], from = 0, to = 1): Vec3[] {
  const getPoint = (t: number): Vec3 => [
    rootCenter[0] + (center[0] - rootCenter[0]) * t,
    rootCenter[1] + (center[1] - rootCenter[1]) * t,
    0,
  ]
  return [getPoint(from), getPoint(to)]
}

// getSpawnStops 让卡片从根卡片的位置出发，升起后滑到自己的位置。
function getSpawnStops(center: [number, number], spawnAt: number): SceneStop[] {
  const start = getIsoTranslate([rootCenter[0] - center[0], rootCenter[1] - center[1], 10])
  return [
    [spawnAt - 0.02, { opacity: 0, transform: start }],
    [spawnAt + 0.01, { opacity: 1, transform: start }],
    [spawnAt + 0.09, { opacity: 1, transform: "translate(0px, 0px)" }],
    [0.9, { opacity: 1, transform: "translate(0px, 0px)" }],
    [0.96, { opacity: 0, transform: "translate(0px, 0px)" }],
  ]
}
</script>

<template>
  <SceneCanvas view-box="-160 -100 312 192" :period="10000">
    <IsoCable
      v-for="subagent in subagents"
      :key="`cable-${subagent.spawnAt}`"
      v-track="getShowStops(subagent.spawnAt + 0.12, 0.88)"
      :points="getCable(subagent.center)"
      tone="lo"
    />
    <!-- 汇报数据包只走两张卡片之间露出来的那一段连线 -->
    <IsoPacket
      :points="getCable(subagents[2].center, 0.38, 0.78)"
      :at="0.6"
      :span="0.1"
      is-reverse
    />

    <IsoSlab
      :at="getCardAt(rootCenter, rootSize)"
      :size="rootSize"
      :radius="9"
      :rows="[[14, 16, 40], [14, 28, 54], [14, 40, 30]]"
    />

    <g v-for="subagent in subagents" :key="subagent.spawnAt" v-track="getSpawnStops(subagent.center, subagent.spawnAt)">
      <IsoSlab :at="getCardAt(subagent.center, subagentSize)" :size="subagentSize" :radius="7" :rows="[[10, 13, 24], [10, 24, 16]]" />
    </g>
  </SceneCanvas>
</template>
