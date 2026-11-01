<script setup lang="ts">
// iso-tools：一个工具托盘上排着两行按键，代表文件、命令和协作工具；
// 信号蓝描边的按键是正在调用的工具，循环里按下、弹起两次，按下时顶面亮起。
import IsoSlab from "../primitives/IsoSlab.vue"
import SceneCanvas from "../primitives/SceneCanvas.vue"
import { project, type Vec3 } from "../primitives/iso"
import { vTrack, type SceneStop } from "../primitives/motion"

const trayAt: Vec3 = [-112, -54, 0]
const traySize: Vec3 = [224, 108, 10]
const keySize: Vec3 = [38, 36, 9]
const keyColumns = [16, 68, 120, 172]
const keyRows = [14, 58]
const liveKey = { column: 2, row: 1 }
const pressDepth = 4

// 每个按键在托盘顶面上的位置；按键下方是略大的凹槽。
const keys = keyRows.flatMap((b, row) =>
  keyColumns.map((a, column) => ({
    key: `${row}-${column}`,
    at: [trayAt[0] + a, trayAt[1] + b, traySize[2]] as Vec3,
    isLive: row === liveKey.row && column === liveKey.column,
    legend: (row * 4 + column) % 3 === 0 ? 18 : 12,
  })),
)

const liveKeyAt = keys.find((key) => key.isLive)!.at
const lightCenter = project([liveKeyAt[0] + keySize[0] / 2, liveKeyAt[1] + keySize[1] / 2, liveKeyAt[2] + keySize[2] - pressDepth])

// getPressStops 让按键在 at 时按下、短暂停住后弹起；按下的时长与灯亮的时长一致。
function getPressStops(presses: number[], down: Keyframe, up: Keyframe): SceneStop[] {
  return presses.flatMap((at) => [
    [at, up],
    [at + 0.03, down],
    [at + 0.11, down],
    [at + 0.16, up],
  ]) as SceneStop[]
}

const presses = [0.2, 0.58]
const keyPressStops = getPressStops(presses, { transform: `translateY(${pressDepth}px)` }, { transform: "translateY(0px)" })
const lightStops = getPressStops(presses, { opacity: 1 }, { opacity: 0 })
</script>

<template>
  <SceneCanvas view-box="-184 -98 368 196" :period="9000">
    <IsoSlab
      :at="trayAt"
      :size="traySize"
      :radius="14"
      :insets="keys.map((key) => [key.at[0] - trayAt[0] - 3, key.at[1] - trayAt[1] - 3, keySize[0] + 6, keySize[1] + 6, 8] as [number, number, number, number, number])"
    />
    <template v-for="key in keys" :key="key.key">
      <g v-if="key.isLive" v-track="keyPressStops">
        <IsoSlab :at="key.at" :size="keySize" :radius="7" tone="hi" :rows="[[11, 18, key.legend]]" row-tone="hi" />
      </g>
      <IsoSlab v-else :at="key.at" :size="keySize" :radius="7" :rows="[[11, 18, key.legend]]" />
    </template>
    <ellipse v-track="lightStops" class="scene-light" :cx="lightCenter[0]" :cy="lightCenter[1]" rx="9" ry="5" />
  </SceneCanvas>
</template>
