<script setup lang="ts">
// iso-cli：一块立着的终端屏，上方两行输出依次出现，最下面是 "$ openvaala" 提示行，
// 行尾信号蓝的光标块像终端插入符一样闪烁；屏前平放一块键盘。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { getRoundedPolylinePath, project, type Vec3 } from "../primitives/iso"
import { getRiseStops, vTrack } from "../primitives/motion"

// 终端屏竖在 x-z 面上，可见面朝 +y；facePoint 把屏幕面上的 (a, b) 换成世界坐标。
const screenAt: Vec3 = [-78, -6, 4]
const screenSize: Vec3 = [156, 80, 6]

function facePoint(a: number, b: number): Vec3 {
  return [screenAt[0] + a, screenAt[1] + screenSize[2], screenAt[2] + b]
}

// faceTransform 让文字和光标贴在屏幕面上：文字横向沿 +x，竖向沿 -z。
function getFaceTransform(a: number, b: number): string {
  const [sx, sy] = project(facePoint(a, b))
  return `matrix(0.866 0.5 0 1 ${sx} ${sy})`
}

const outputRows = [
  { b: 56, length: 92, appearAt: 0.1 },
  { b: 44, length: 64, appearAt: 0.3 },
]

function getRowPath(b: number, length: number): string {
  return getRoundedPolylinePath([facePoint(16, b), facePoint(16 + length, b)], 0)
}
</script>

<template>
  <SceneCanvas view-box="-172 -129 320 192" :period="8000">
    <IsoSlab :at="screenAt" :size="screenSize" plane="wall-x" :radius="9" :insets="[[6, 6, screenSize[0] - 12, screenSize[1] - 12, 5]]" />
    <path
      v-for="row in outputRows"
      :key="row.b"
      v-track="getRiseStops(row.appearAt, 0.92, 2)"
      class="iso-row"
      :d="getRowPath(row.b, row.length)"
    />
    <g :transform="getFaceTransform(16, 22)">
      <text class="cli-prompt" x="0" y="0"><tspan class="cli-prompt-sign">$</tspan> openvaala</text>
      <rect class="cli-caret" x="82" y="-10" width="7" height="12" />
    </g>

    <!-- 屏前的键盘 -->
    <IsoSlab
      :at="[-56, 22, 0]"
      :size="[112, 40, 6]"
      :radius="8"
      :insets="[[8, 8, 96, 24, 5]]"
      :rows="[[16, 14, 80], [16, 20, 80], [30, 26, 52]]"
    />
  </SceneCanvas>
</template>

<style scoped>
.cli-prompt {
  fill: var(--ink-soft);
  font-family: var(--font-code);
  font-size: 12px;
}

.cli-prompt-sign {
  fill: var(--ink-muted);
}

.cli-caret {
  fill: var(--scene-hi);
  animation: caret-blink 1.6s steps(1, end) infinite;
}
</style>
