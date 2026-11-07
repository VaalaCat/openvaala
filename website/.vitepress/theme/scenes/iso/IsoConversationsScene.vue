<script setup lang="ts">
// iso-conversations：一排消息卡片按时间依次出现，连成主线；随后从第二张卡片分出一条支线，
// 分叉后的第一张卡片（信号蓝）和后续卡片浮现。静态画面是主线和支线都已完整。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoCable from "../primitives/IsoCable.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { project, type Vec3 } from "../primitives/iso"
import { getRiseStops, getShowStops, vTrack } from "../primitives/motion"

const { localeIndex } = useData()
const forkLabel = computed(() => (localeIndex.value === "zh" ? "分叉" : "fork"))

const cardSize: Vec3 = [52, 40, 4]
const cardRows: [number, number, number][] = [
  [8, 14, 32],
  [8, 25, 22],
]

// 主线沿 x 排开，每张卡在 shownAt 升起；卡片停稳后才画出它与前一张之间的连线，循环末尾连线先于卡片淡出。
const mainCards = [-150, -86, -22, 42].map((x, index) => ({ at: [x, 20, 0] as Vec3, shownAt: 0.04 + index * 0.07 }))
const mainLinks = mainCards.slice(1).map((card, index) => ({
  points: [
    [mainCards[index].at[0] + cardSize[0], 40, 2],
    [card.at[0], 40, 2],
  ] as Vec3[],
  shownAt: card.shownAt,
}))

// 支线从第二张卡的后沿沿 -y 分出。
const forkAt = 0.4
const branchCards = [-44, -108].map((y, index) => ({ at: [-86, y, 0] as Vec3, shownAt: forkAt + 0.06 + index * 0.1 }))
const branchLinks = [
  { points: [[-60, 20, 2], [-60, -4, 2]] as Vec3[], shownAt: branchCards[0].shownAt },
  { points: [[-60, -44, 2], [-60, -68, 2]] as Vec3[], shownAt: branchCards[1].shownAt },
]
// 标签贴在分叉卡片的右角外侧。
const forkLabelAt = project([-34, -44, 4])
</script>

<template>
  <SceneCanvas view-box="-196 -114 274 204" :period="10000">
    <g v-for="link in branchLinks" :key="link.shownAt" v-track="getShowStops(link.shownAt + 0.03, 0.9, 0.03)">
      <IsoCable :points="link.points" />
    </g>
    <g v-track="getRiseStops(branchCards[1].shownAt, 0.93)">
      <IsoSlab :at="branchCards[1].at" :size="cardSize" :radius="7" :rows="cardRows" />
    </g>
    <g v-track="getRiseStops(branchCards[0].shownAt, 0.93)">
      <IsoSlab :at="branchCards[0].at" :size="cardSize" :radius="7" :rows="cardRows" tone="hi" />
    </g>

    <g v-for="link in mainLinks" :key="link.shownAt" v-track="getShowStops(link.shownAt + 0.03, 0.9, 0.03)">
      <IsoCable :points="link.points" />
    </g>
    <g v-for="card in mainCards" :key="card.shownAt" v-track="getRiseStops(card.shownAt, 0.93)">
      <IsoSlab :at="card.at" :size="cardSize" :radius="7" :rows="cardRows" />
    </g>

    <text
      v-track="getShowStops(branchCards[0].shownAt, 0.93, 0.03)"
      class="scene-label"
      :x="forkLabelAt[0] + 8"
      :y="forkLabelAt[1] + 8"
      text-anchor="start"
    >
      {{ forkLabel }}
    </text>
  </SceneCanvas>
</template>
