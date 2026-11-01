<script setup lang="ts">
// iso-install：一个包装盒从上方落进底座的落位框，轻轻一顿后停稳，底座前沿的指示灯亮起表示安装完成。
// 静态画面是盒子已落位、指示灯亮着。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getIsoPrism, project, type Vec3 } from "../primitives/iso"
import { getShowStops, vTrack, type SceneStop } from "../primitives/motion"

const boxAt: Vec3 = [-34, -34, 10]
const boxSize: Vec3 = [68, 68, 50]
const shadowPath = getIsoPrism("floor", boxAt, [boxSize[0], boxSize[1], 0], 6).silhouette
const lightCenter = project([56, 56, 10])

// 盒子先加速落下，触底后轻轻弹起再停稳，循环末尾淡出。
const dropStops: SceneStop[] = [
  [0.04, { opacity: 0, transform: "translateY(-30px)" }],
  [0.1, { opacity: 1, transform: "translateY(-24px)", easing: "cubic-bezier(0.55, 0, 0.8, 0.3)" }],
  [0.24, { opacity: 1, transform: "translateY(0px)", easing: "cubic-bezier(0.2, 0.8, 0.2, 1)" }],
  [0.27, { opacity: 1, transform: "translateY(-2.5px)", easing: "ease-in" }],
  [0.3, { opacity: 1, transform: "translateY(0px)" }],
  [0.92, { opacity: 1, transform: "translateY(0px)" }],
  [0.96, { opacity: 0, transform: "translateY(0px)" }],
]

// 落下过程中影子从小变大、由浅变深。
const shadowStops: SceneStop[] = [
  [0.08, { opacity: 0, transform: "scale(0.7)" }],
  [0.24, { opacity: 1, transform: "scale(1)" }],
  [0.92, { opacity: 1, transform: "scale(1)" }],
  [0.96, { opacity: 0, transform: "scale(1)" }],
]
</script>

<template>
  <SceneCanvas view-box="-150 -124 300 214" :period="9000">
    <IsoSlab :at="[-75, -75, 0]" :size="[150, 150, 10]" :radius="18" :insets="[[36, 36, 78, 78, 9]]" />
    <path v-track="shadowStops" class="drop-shadow" :d="shadowPath" />
    <g v-track="dropStops">
      <IsoSlab :at="boxAt" :size="boxSize" :radius="5" has-corner-edge :rows="[[6, 34, 56]]" />
    </g>
    <ellipse
      v-track="getShowStops(0.31, 0.92, 0.02)"
      class="install-light"
      :cx="lightCenter[0]"
      :cy="lightCenter[1]"
      rx="5"
      ry="2.9"
    />
  </SceneCanvas>
</template>

<style scoped>
.drop-shadow {
  fill: color-mix(in srgb, var(--ink) 7%, transparent);
  stroke: none;
  transform-box: fill-box;
  transform-origin: center;
}

.install-light {
  fill: var(--scene-hi);
}
</style>
