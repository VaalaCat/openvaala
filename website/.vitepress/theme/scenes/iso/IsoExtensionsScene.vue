<script setup lang="ts">
// iso-extensions：一块扩展底板上有四条插槽，Skill、MCP、Hook、插件四种扩展像板卡一样依次落下插好；
// 每插好一张，底板前角的信号蓝指示灯闪一下。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { project, type Vec3 } from "../primitives/iso"
import { vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const isZh = computed(() => localeIndex.value === "zh")

const boardAt: Vec3 = [-80, -96, 0]
const boardSize: Vec3 = [160, 192, 8]
const cardWidth = 104
const cardHeight = 46
const cardThickness = 6
const dropHeight = 34

// 四张扩展卡从后往前排列；clickAt 是卡片落到底的时刻。
const cards = computed(() => [
  { label: isZh.value ? "Skill" : "skills", b: 24, clickAt: 0.12 },
  { label: "MCP", b: 68, clickAt: 0.26 },
  { label: isZh.value ? "Hook" : "hooks", b: 112, clickAt: 0.4 },
  { label: isZh.value ? "插件" : "plugins", b: 156, clickAt: 0.54 },
])

function getCardAt(b: number): Vec3 {
  return [boardAt[0] + 28, boardAt[1] + b, boardSize[2]]
}

// 标签放在每张卡片右下角外侧的底板上，不会压到后面的卡片。
function getLabelPosition(b: number) {
  const [x, y, z] = getCardAt(b)
  const [sx, sy] = project([x + cardWidth, y + cardThickness, z])
  return { x: sx + 7, y: sy + 1 }
}

// getDropStops 让卡片从上方落下，到底时轻轻回弹一下，循环末尾淡出。
function getDropStops(clickAt: number): SceneStop[] {
  return [
    [clickAt - 0.09, { opacity: 0, transform: `translateY(${-dropHeight}px)` }],
    [clickAt - 0.06, { opacity: 1, transform: `translateY(${-dropHeight}px)` }],
    [clickAt, { opacity: 1, transform: "translateY(1.5px)", easing: "ease-out" }],
    [clickAt + 0.025, { opacity: 1, transform: "translateY(0px)" }],
    [0.9, { opacity: 1, transform: "translateY(0px)" }],
    [0.96, { opacity: 0, transform: "translateY(0px)" }],
  ]
}

const lightCenter = project([boardAt[0] + boardSize[0] - 12, boardAt[1] + boardSize[1] - 12, boardSize[2]])
const lightStops = computed<SceneStop[]>(() =>
  cards.value.flatMap((card) => [
    [card.clickAt, { opacity: 0 }],
    [card.clickAt + 0.01, { opacity: 1 }],
    [card.clickAt + 0.06, { opacity: 0 }],
  ]) as SceneStop[],
)
</script>

<template>
  <SceneCanvas view-box="-196 -112 392 220" :period="10000">
    <IsoSlab
      :at="boardAt"
      :size="boardSize"
      :radius="12"
      :insets="cards.map((card) => [24, card.b - 2, cardWidth + 8, cardThickness + 4, 3] as [number, number, number, number, number])"
    />
    <ellipse v-track="lightStops" class="scene-light" :cx="lightCenter[0]" :cy="lightCenter[1]" rx="4.5" ry="2.6" />
    <g v-for="card in cards" :key="card.b" v-track="getDropStops(card.clickAt)">
      <IsoSlab
        :at="getCardAt(card.b)"
        :size="[cardWidth, cardHeight, cardThickness]"
        plane="wall-x"
        :radius="5"
        :rows="[[12, cardHeight - 14, 34], [12, cardHeight - 24, 52]]"
      />
      <text class="scene-label scene-label--ink" :x="getLabelPosition(card.b).x" :y="getLabelPosition(card.b).y">
        {{ card.label }}
      </text>
    </g>
  </SceneCanvas>
</template>
