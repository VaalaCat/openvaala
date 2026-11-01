<script setup lang="ts">
// iso-editors：平放的编辑器（行号栏和代码行），侧面有一个插口；插头顺着线缆滑进插口，
// 接通后插口上的指示灯亮起，线缆另一端是 OpenVaala 运行时。静态画面是插头已插好、指示灯亮着。
import { computed } from "vue"
import { useData } from "vitepress"
import IsoCable from "../primitives/IsoCable.vue"
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoTranslate, project, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const { localeIndex } = useData()
const runtimeLabel = computed(() => (localeIndex.value === "zh" ? "openvaala 运行时" : "openvaala runtime"))

const codeRows: [number, number, number][] = [70, 96, 52, 110, 84, 62, 100, 40, 76].flatMap((length, index) => [
  [8, 14 + index * 10, 5] as [number, number, number],
  [20, 14 + index * 10, length] as [number, number, number],
])

const cable: Vec3[] = [
  [66, -16, 5],
  [96, -16, 5],
  [96, 28, 5],
  [112, 28, 5],
]

// 插头从线缆上滑过来插进插口，停稳后指示灯亮起，循环末尾拔出。
const plugStops: SceneStop[] = [
  [0.06, { transform: getIsoTranslate([34, 0, 0]) }],
  [0.26, { transform: getIsoTranslate([0, 0, 0]), easing: "cubic-bezier(0.2, 0.8, 0.2, 1)" }],
  [0.88, { transform: getIsoTranslate([0, 0, 0]) }],
  [0.98, { transform: getIsoTranslate([34, 0, 0]) }],
]

const lightCenter = project([33, -16, 10])
const zedLabel = project([30, 46, 0])
const runtimeLabelAt = project([146, 46, 0])
</script>

<template>
  <SceneCanvas view-box="-158 -112 290 228" :period="9000">
    <IsoSlab :at="[-120, -70, 0]" :size="[150, 116, 8]" :radius="10" :rows="codeRows" />
    <IsoSlab :at="[30, -26, 0]" :size="[6, 20, 10]" :radius="2" />

    <IsoCable :points="cable" />
    <IsoSlab :at="[110, 10, 0]" :size="[36, 36, 20]" :radius="8" has-corner-edge :insets="[[9, 9, 18, 18, 9]]" />
    <g v-track="plugStops">
      <IsoSlab :at="[40, -24, 0]" :size="[26, 16, 12]" :radius="3" has-corner-edge />
    </g>

    <ellipse v-track="getShowStops(0.27, 0.88, 0.02)" class="socket-light" :cx="lightCenter[0]" :cy="lightCenter[1]" rx="2.6" ry="1.6" />

    <text class="scene-label scene-label--ink" :x="zedLabel[0]" :y="zedLabel[1] + 16" text-anchor="middle">ACP · Zed</text>
    <text class="scene-label" :x="runtimeLabelAt[0]" :y="runtimeLabelAt[1] + 14" text-anchor="middle">{{ runtimeLabel }}</text>
  </SceneCanvas>
</template>

<style scoped>
.socket-light {
  fill: var(--scene-hi);
}
</style>
