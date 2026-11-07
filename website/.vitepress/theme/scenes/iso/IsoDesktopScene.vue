<script setup lang="ts">
// iso-desktop：平放的桌面，程序坞从前沿升起，两扇窗口依次浮现并叠在一起，
// 最后坞上 OpenVaala 图标下的运行指示灯亮起。静态画面是窗口和程序坞都在、指示灯亮着。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoPrism, project, type Vec3 } from "../primitives/iso"
import { getRiseStops, getShowStops, vTrack } from "../primitives/motion"

const backWindowAt: Vec3 = [-96, -58, 6]
const frontWindowAt: Vec3 = [-34, -30, 16]
const windowSize: Vec3 = [112, 76, 4]
const windowInsets: [number, number, number, number, number][] = [
  [6, 6, 4, 4, 2],
  [12, 6, 4, 4, 2],
  [18, 6, 4, 4, 2],
  [6, 16, 100, 54, 4],
]

// 前窗悬在桌面上方，桌面上画出它的影子。
const frontShadow = getIsoPrism("floor", [frontWindowAt[0] + 4, frontWindowAt[1] + 4, 6], [windowSize[0], windowSize[1], 0], 8).silhouette

const dockAt: Vec3 = [-62, 46, 6]
const dockSize: Vec3 = [118, 18, 5]
// 坞上五个图标沿 x 排开；运行指示灯在中间图标的正前方，和图标、坞的前沿各留出一点距离。
const dockIcons = [0, 1, 2, 3, 4].map((index) => [7 + index * 22, 3, 9, 8, 2.5] as [number, number, number, number, number])
const lightCenter = project([dockAt[0] + 7 + 2 * 22 + 4.5, dockAt[1] + 14.5, dockAt[2] + dockSize[2]])
</script>

<template>
  <SceneCanvas view-box="-168 -110 336 214" :period="10000">
    <IsoSlab :at="[-110, -70, 0]" :size="[220, 140, 6]" :radius="14" />

    <g v-track="getRiseStops(0.18, 0.93, 6)">
      <IsoSlab :at="backWindowAt" :size="windowSize" :radius="7" :insets="windowInsets" :rows="[[12, 30, 60], [12, 40, 82], [12, 50, 44]]" />
    </g>
    <path v-track="getShowStops(0.3, 0.93, 0.04)" class="window-shadow" :d="frontShadow" />
    <g v-track="getRiseStops(0.3, 0.93, 8)">
      <IsoSlab :at="frontWindowAt" :size="windowSize" :radius="7" :insets="windowInsets" :rows="[[12, 30, 72], [12, 40, 50], [12, 50, 88]]" />
    </g>

    <g v-track="getRiseStops(0.06, 0.93, 10)">
      <IsoSlab :at="dockAt" :size="dockSize" :radius="7" :insets="dockIcons" />
    </g>
    <ellipse
      v-track="getShowStops(0.42, 0.93, 0.02)"
      class="dock-light"
      :cx="lightCenter[0]"
      :cy="lightCenter[1]"
      rx="2.6"
      ry="1.5"
    />
  </SceneCanvas>
</template>

<style scoped>
.window-shadow {
  fill: color-mix(in srgb, var(--ink) 6%, transparent);
  stroke: none;
}

.dock-light {
  fill: var(--scene-hi);
}
</style>
